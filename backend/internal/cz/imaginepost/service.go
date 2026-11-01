// Package imaginepost is CodizeLabs fork patch P3 (ADR-0002): Grok Imagine
// posts as first-class resources. Each post is one grok.com conversation,
// pinned for life to the account that created it; every later call for it
// runs on that account or not at all. See README.md for the endpoints, the
// allow-list and the upstream files P3 touches.
package imaginepost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/web"
	"gorm.io/gorm"
)

// Transport is the one way P3 reaches grok.com (web.Adapter implements it).
type Transport interface {
	DoImagine(ctx context.Context, credential account.Credential, method, path string, body []byte) (*http.Response, error)
	UploadImagine(ctx context.Context, credential account.Credential, filename, mimeType string, data []byte) (string, error)
	// SignImagine obtains the Statsig signature for the send at method+path
	// before any upload of that send (fails closed).
	SignImagine(ctx context.Context, credential account.Credential, method, path string) error
	FetchImagineAsset(ctx context.Context, credential account.Credential, key string) (io.ReadCloser, string, error)
}

// Purpose selects the route and quota mode an account is leased for.
type Purpose string

const (
	// PurposeImage covers creates, edits and reads.
	PurposeImage Purpose = "image"
	// PurposeVideo covers video sends.
	PurposeVideo Purpose = "video"
	// PurposeRead covers free reads: no quota mode.
	PurposeRead Purpose = "read"
)

// Lease is one leased account.
type Lease interface {
	Credential() account.Credential
	Release()
}

// Candidate is one account a new post may be pinned to, with the quota
// grok2api already holds for it (-1 means unknown).
type Candidate struct {
	AccountID          uint64
	ImageEditRemaining int
	Video720pRemaining int
}

// Pool leases accounts. AcquirePinned must return only accountID or an error.
// NewPostCandidates lists accounts without leasing or calling grok.com.
type Pool interface {
	AcquirePinned(ctx context.Context, accountID uint64, purpose Purpose, scope clientkeydomain.AccountScope) (Lease, error)
	NewPostCandidates(ctx context.Context, scope clientkeydomain.AccountScope) ([]Candidate, error)
	Report(ctx context.Context, credential account.Credential, status int)
}

// Config holds P3's switches (config key cz.imaginePost).
type Config struct {
	// AllowLike opens like/unlike on the allow-list. Off by default (§ 6 Q4).
	AllowLike bool
	// PinnedOnlyAccountIDs serve only posts already pinned to them and are
	// never chosen for a new post (ADR-0002 § 1).
	PinnedOnlyAccountIDs []uint64
	// MaxImageBytes caps one uploaded reference.
	MaxImageBytes int
	// SendTimeout bounds one send's stream.
	SendTimeout time.Duration
	// ReadTimeout bounds one free read.
	ReadTimeout time.Duration
	// DebugReplyShapeFile, when set, appends each send's reply key structure
	// (no values, prompts, URLs or ids) to this file (replyshape.go). Off by
	// default.
	DebugReplyShapeFile string
}

func (c Config) withDefaults() Config {
	if c.MaxImageBytes <= 0 {
		c.MaxImageBytes = 20 << 20
	}
	if c.SendTimeout <= 0 {
		c.SendTimeout = 10 * time.Minute
	}
	if c.ReadTimeout <= 0 {
		c.ReadTimeout = time.Minute
	}
	return c
}

// Caller is the authenticated client key.
type Caller struct {
	ClientKeyID uint64
	Scope       clientkeydomain.AccountScope
}

// Options builds a Service.
type Options struct {
	DB          *gorm.DB
	Pool        Pool
	Transport   Transport
	LabelSecret string
	Logger      *slog.Logger
	Config      Config
}

// Service runs every P3 operation.
type Service struct {
	store     *Store
	pool      Pool
	transport Transport
	labels    *Labeler
	logger    *slog.Logger
	config    atomic.Pointer[Config]

	busyMu sync.Mutex
	busy   map[uint64]bool
}

// New migrates P3's tables and returns the service.
func New(ctx context.Context, options Options) (*Service, error) {
	if options.DB == nil || options.Pool == nil || options.Transport == nil || options.LabelSecret == "" {
		return nil, errors.New("imaginepost: DB, Pool, Transport and LabelSecret are required")
	}
	store := NewStore(options.DB)
	if err := store.Migrate(ctx); err != nil {
		return nil, err
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	service := &Service{store: store, pool: options.Pool, transport: options.Transport, labels: NewLabeler(options.LabelSecret), logger: logger, busy: map[uint64]bool{}}
	service.UpdateConfig(options.Config)
	return service, nil
}

// UpdateConfig swaps P3's switches.
func (s *Service) UpdateConfig(config Config) {
	config = config.withDefaults()
	s.config.Store(&config)
}

func (s *Service) cfg() Config { return *s.config.Load() }

// Store exposes P3's persistence (tests and diagnostics).
func (s *Service) Store() *Store { return s.store }

// Image is one reference image sent with a create or edit.
type Image struct {
	Filename string
	MIMEType string
	Data     []byte
}

// CreateInput is the first send of a new post.
type CreateInput struct {
	JobID       string
	Prompt      string
	Image       *Image
	AspectRatio string
}

// EditInput is an edit inside a post on a chosen item.
type EditInput struct {
	JobID         string
	Prompt        string
	SourceAssetID string
	Parent        string // "head" (default), "source" or a response id
	Extra         []Image
	AspectRatio   string
}

// VideoInput is a video inside a post from a chosen image item.
type VideoInput struct {
	JobID         string
	Prompt        string
	SourceAssetID string
	Parent        string
	DurationS     int
	Resolution    string
	AspectRatio   string
	Mode          string
}

// SendResult is what a send produced. Ids here are the layer's provider_ref.
type SendResult struct {
	PostID           string   `json:"post_id"`
	AccountLabel     string   `json:"account_label"`
	Kind             string   `json:"kind"`
	ResponseID       string   `json:"response_id"`
	ParentResponseID string   `json:"parent_response_id,omitempty"`
	SourceAssetID    string   `json:"source_asset_id,omitempty"`
	AssetIDs         []string `json:"asset_ids"`
	ContextVerified  bool     `json:"context_verified"`
	DurationS        int      `json:"duration_s,omitempty"`
	Resolution       string   `json:"resolution,omitempty"`
}

// ItemFacts is one free read of an item's asset record.
type ItemFacts struct {
	PostID        string `json:"post_id"`
	AssetID       string `json:"asset_id"`
	ResponseID    string `json:"response_id,omitempty"`
	MIMEType      string `json:"mime_type"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	OwnedByPinned *bool  `json:"owned_by_pinned_account,omitempty"`
	IsPublic      bool   `json:"is_public"`
	Shared        bool   `json:"shared"`
	InPost        bool   `json:"in_post"`
	AccountLabel  string `json:"account_label"`
	assetKey      string
}

var (
	jobIDPattern  = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	aspectPattern = regexp.MustCompile(`^[0-9]{1,2}:[0-9]{1,2}$`)
	modePattern   = regexp.MustCompile(`^[a-z]{1,16}$`)
)

const maxPromptRunes = 8000

func validPrompt(prompt string) bool {
	trimmed := strings.TrimSpace(prompt)
	return trimmed != "" && len([]rune(trimmed)) <= maxPromptRunes
}

func (s *Service) validImage(image Image) bool {
	return strings.HasPrefix(strings.ToLower(image.MIMEType), "image/") && len(image.Data) > 0 && len(image.Data) <= s.cfg().MaxImageBytes
}

func invalid() *Error { return notSent(CodeInvalidRequest, http.StatusBadRequest) }

// CreatePost makes a new post: upload the reference, then conversations/new
// on an account the pool picks. The post is pinned to that account for life.
func (s *Service) CreatePost(ctx context.Context, caller Caller, input CreateInput) (*SendResult, error) {
	if input.JobID == "" {
		return nil, notSent(CodeJobIDRequired, http.StatusBadRequest)
	}
	if !jobIDPattern.MatchString(input.JobID) || !validPrompt(input.Prompt) || (input.AspectRatio != "" && !aspectPattern.MatchString(input.AspectRatio)) {
		return nil, invalid()
	}
	if input.Image == nil {
		// Text-only creation uses the imagine WebSocket, which P3 doesn't
		// open; a post starts from an uploaded anchor.
		return nil, notSent(CodeCapabilityUnsupported, http.StatusUnprocessableEntity)
	}
	if !s.validImage(*input.Image) {
		return nil, invalid()
	}
	reserved, err := s.store.ReserveJob(ctx, caller.ClientKeyID, input.JobID, "create", "")
	if err != nil {
		return nil, s.logged("create", "", 0, notSent(CodeInternal, http.StatusInternalServerError))
	}
	if !reserved {
		return nil, s.logged("create", "", 0, notSent(CodeJobIDReused, http.StatusConflict))
	}
	state := string(NotSent)
	postRef := ""
	defer func() {
		_ = s.store.FinishJob(context.WithoutCancel(ctx), caller.ClientKeyID, input.JobID, state, postRef)
	}()
	lease, release, chooseErr := s.chooseNewAccount(ctx, caller)
	if chooseErr != nil {
		return nil, s.logged("create", "", 0, chooseErr)
	}
	defer lease.Release()
	defer release()
	credential := lease.Credential()

	if signErr := s.signSend(ctx, credential, pathCreate); signErr != nil {
		return nil, s.logged("create", "", credential.ID, signErr)
	}
	fileID, uploadErr := s.upload(ctx, credential, *input.Image)
	if uploadErr != nil {
		return nil, s.logged("create", "", credential.ID, uploadErr)
	}
	stream, sendErr := s.send(ctx, credential, "create", "", pathCreate, buildCreateBody(input.Prompt, []string{fileID}, input.AspectRatio))
	if sendErr != nil {
		state = string(sendErr.Send)
		// A send that may have made a post still pins it, so a later free
		// read on the same account can find out what happened.
		if sendErr.Send != NotSent && len(stream.ConversationIDs) > 0 && validID.MatchString(stream.ConversationIDs[0]) {
			conversationID := stream.ConversationIDs[0]
			postRef = s.labels.Ref(conversationID)
			if err := s.store.PutOwner(context.WithoutCancel(ctx), PostOwner{ConversationID: conversationID, AccountID: credential.ID, ClientKeyID: caller.ClientKeyID}); err != nil {
				s.logger.Error("cz_imagine_owner_write_failed", "post", postRef)
			}
			sendErr.Result = &SendResult{PostID: conversationID, AccountLabel: s.labels.AccountLabel(credential.ID), Kind: "image", AssetIDs: stream.AssetIDs}
		}
		return nil, s.logged("create", "", credential.ID, sendErr)
	}
	state = string(Sent)
	if len(stream.ConversationIDs) == 0 || !validID.MatchString(stream.ConversationIDs[0]) || len(stream.AssetIDs) == 0 {
		return nil, s.logged("create", "", credential.ID, fail(CodeGenerationFailed, http.StatusBadGateway, Sent))
	}
	conversationID := stream.ConversationIDs[0]
	postRef = s.labels.Ref(conversationID)
	result := &SendResult{
		PostID: conversationID, AccountLabel: s.labels.AccountLabel(credential.ID), Kind: "image",
		ResponseID: stream.ResponseID, AssetIDs: stream.AssetIDs,
	}
	owner := PostOwner{ConversationID: conversationID, AccountID: credential.ID, ClientKeyID: caller.ClientKeyID}
	record, readErr := s.readAsset(ctx, credential, conversationID, stream.AssetIDs[0])
	if readErr == nil {
		owner.OwnerUserHash = s.labels.UserHash(record.OwnerUserID)
		result.ContextVerified = record.conversation() == conversationID
	}
	if err := s.store.PutOwner(context.WithoutCancel(ctx), owner); err != nil {
		s.logger.Error("cz_imagine_owner_write_failed", "post", postRef, "account", result.AccountLabel)
	}
	if len(stream.ConversationIDs) > 1 || (readErr == nil && !result.ContextVerified) {
		return nil, s.logged("create", conversationID, credential.ID, &Error{Code: CodePostContextLost, Status: http.StatusBadGateway, Send: Sent, Result: result})
	}
	s.logOK("create", conversationID, credential.ID)
	return result, nil
}

// maxNewAccountTries bounds how many free quota reads one create makes.
const maxNewAccountTries = 4

// rankCandidates orders the accounts a new post may go to (ADR-0002 § 1):
// fewest pinned posts, then most remaining image-edit quota, then most
// remaining 720p video quota (unknown, -1, after any known value), then the
// lowest account id. The order is deterministic.
func rankCandidates(candidates []Candidate, openPosts map[uint64]int) {
	sort.SliceStable(candidates, func(left, right int) bool {
		a, b := candidates[left], candidates[right]
		if openPosts[a.AccountID] != openPosts[b.AccountID] {
			return openPosts[a.AccountID] < openPosts[b.AccountID]
		}
		if a.ImageEditRemaining != b.ImageEditRemaining {
			return a.ImageEditRemaining > b.ImageEditRemaining
		}
		if a.Video720pRemaining != b.Video720pRemaining {
			return a.Video720pRemaining > b.Video720pRemaining
		}
		return a.AccountID < b.AccountID
	})
}

// chooseNewAccount picks the account a new post is pinned to: the best
// ranked candidate inside the key's scope that isn't pinned-only, has no send
// in flight, is leased by the pool (not cooling, not out of quota) and whose
// free quota read confirms 15 s at 720p, so every later video in the post can
// be made on it. A busy or refused account is skipped for the next. It returns
// the lease and the account's busy claim; the caller releases both.
func (s *Service) chooseNewAccount(ctx context.Context, caller Caller) (Lease, func(), *Error) {
	candidates, err := s.pool.NewPostCandidates(ctx, caller.Scope)
	if err != nil {
		return nil, nil, mapAcquireErr(err, CodeNoAccount)
	}
	openPosts, err := s.store.OpenPostsByAccount(ctx)
	if err != nil {
		return nil, nil, notSent(CodeInternal, http.StatusInternalServerError)
	}
	pinnedOnly := map[uint64]bool{}
	for _, id := range s.cfg().PinnedOnlyAccountIDs {
		pinnedOnly[id] = true
	}
	eligible := candidates[:0:0]
	for _, candidate := range candidates {
		if candidate.AccountID != 0 && !pinnedOnly[candidate.AccountID] {
			eligible = append(eligible, candidate)
		}
	}
	rankCandidates(eligible, openPosts)

	busySkips, otherSkips, tries := 0, 0, 0
	var refused *Error
	for _, candidate := range eligible {
		if tries >= maxNewAccountTries {
			break
		}
		release, busyErr := s.claimAccount(candidate.AccountID)
		if busyErr != nil {
			busySkips++
			continue
		}
		lease, acquireErr := s.pool.AcquirePinned(ctx, candidate.AccountID, PurposeImage, caller.Scope)
		if acquireErr != nil {
			release()
			otherSkips++
			if mapped := mapAcquireErr(acquireErr, CodeNoAccount); refused == nil || mapped.Status == http.StatusTooManyRequests {
				refused = mapped
			}
			continue
		}
		if lease.Credential().ID != candidate.AccountID {
			// The pool broke its contract; never pin a post to another account.
			lease.Release()
			release()
			otherSkips++
			continue
		}
		tries++
		quota, quotaErr := s.quota(ctx, lease.Credential(), "")
		if quotaErr == nil && quota.MaxVideoSeconds >= videoDurationSeconds && quota.Video720p {
			return lease, release, nil
		}
		lease.Release()
		release()
		if quotaErr != nil && quotaErr.Code == CodeStatsigUnavailable {
			return nil, nil, quotaErr
		}
		otherSkips++
	}
	if busySkips > 0 && otherSkips == 0 && tries == 0 {
		return nil, nil, errAccountBusy()
	}
	if refused != nil && tries == 0 {
		return nil, nil, refused
	}
	return nil, nil, notSent(CodeNoAccount, http.StatusServiceUnavailable)
}

// resolvePost checks the caller's pin and label before anything leaves P3.
func (s *Service) resolvePost(ctx context.Context, caller Caller, conversationID, label string) (PostOwner, *Error) {
	if !validID.MatchString(conversationID) {
		return PostOwner{}, notSent(CodePostNotFound, http.StatusNotFound)
	}
	if strings.TrimSpace(label) == "" {
		return PostOwner{}, notSent(CodeAccountLabelRequired, http.StatusBadRequest)
	}
	owner, err := s.store.Owner(ctx, conversationID)
	if errors.Is(err, errNotFound) || (err == nil && owner.ClientKeyID != caller.ClientKeyID) {
		return PostOwner{}, notSent(CodePostNotFound, http.StatusNotFound)
	}
	if err != nil {
		return PostOwner{}, notSent(CodeInternal, http.StatusInternalServerError)
	}
	if label != s.labels.AccountLabel(owner.AccountID) {
		return PostOwner{}, notSent(CodeAccountLabelMismatch, http.StatusConflict)
	}
	return owner, nil
}

// acquirePinned leases exactly the post's account, never another.
func (s *Service) acquirePinned(ctx context.Context, caller Caller, owner PostOwner, purpose Purpose) (Lease, *Error) {
	lease, err := s.pool.AcquirePinned(ctx, owner.AccountID, purpose, caller.Scope)
	if err != nil {
		return nil, mapAcquireErr(err, CodePinnedAccountUnavailable)
	}
	if lease.Credential().ID != owner.AccountID {
		// The pool broke its contract; refuse rather than send elsewhere.
		lease.Release()
		return nil, notSent(CodePinnedAccountUnavailable, http.StatusServiceUnavailable)
	}
	return lease, nil
}

// EditInPost edits a chosen item inside the post, on the post's account.
func (s *Service) EditInPost(ctx context.Context, caller Caller, conversationID, label string, input EditInput) (*SendResult, error) {
	if input.JobID == "" {
		return nil, notSent(CodeJobIDRequired, http.StatusBadRequest)
	}
	if !jobIDPattern.MatchString(input.JobID) || !validPrompt(input.Prompt) || !validID.MatchString(input.SourceAssetID) ||
		len(input.Extra) > 4 || (input.AspectRatio != "" && !aspectPattern.MatchString(input.AspectRatio)) {
		return nil, invalid()
	}
	for _, image := range input.Extra {
		if !s.validImage(image) {
			return nil, invalid()
		}
	}
	return s.sendInPost(ctx, caller, conversationID, label, "edit", input.JobID, input.SourceAssetID, input.Parent, PurposeImage,
		func(ctx context.Context, credential account.Credential, snapshot PostSnapshot, parent string) ([]byte, *Error) {
			assets := []string{input.SourceAssetID}
			for _, image := range input.Extra {
				id, err := s.upload(ctx, credential, image)
				if err != nil {
					return nil, err
				}
				assets = append(assets, id)
			}
			return buildEditBody(input.Prompt, parent, assets, input.AspectRatio), nil
		}, "image", 0, "")
}

// VideoInPost makes a 15 s 720p video inside the post from a chosen image
// item. Any other spec is refused before any call; an account whose free
// quota read can't confirm 15 s at 720p is refused before the send.
func (s *Service) VideoInPost(ctx context.Context, caller Caller, conversationID, label string, input VideoInput) (*SendResult, error) {
	if input.JobID == "" {
		return nil, notSent(CodeJobIDRequired, http.StatusBadRequest)
	}
	if !jobIDPattern.MatchString(input.JobID) || !validPrompt(input.Prompt) || !validID.MatchString(input.SourceAssetID) ||
		(input.AspectRatio != "" && !aspectPattern.MatchString(input.AspectRatio)) || (input.Mode != "" && !modePattern.MatchString(input.Mode)) {
		return nil, invalid()
	}
	if input.DurationS != videoDurationSeconds || input.Resolution != videoResolution {
		return nil, notSent(CodeSpecUnsupported, http.StatusUnprocessableEntity)
	}
	return s.sendInPost(ctx, caller, conversationID, label, "video", input.JobID, input.SourceAssetID, input.Parent, PurposeVideo,
		func(ctx context.Context, credential account.Credential, snapshot PostSnapshot, parent string) ([]byte, *Error) {
			if fact, ok := snapshot.asset(input.SourceAssetID); ok && fact.MIMEType != "" && !strings.HasPrefix(fact.MIMEType, "image/") {
				return nil, notSent(CodeInvalidRequest, http.StatusUnprocessableEntity)
			}
			quota, err := s.quota(ctx, credential, conversationID)
			if err != nil {
				return nil, err
			}
			if quota.MaxVideoSeconds < videoDurationSeconds || !quota.Video720p {
				return nil, notSent(CodeSpecUnconfirmable, http.StatusUnprocessableEntity)
			}
			if quota.Available["video720p"] <= 0 {
				return nil, &Error{Code: CodePinnedAccountUnavailable, Status: http.StatusTooManyRequests, Send: NotSent}
			}
			return buildVideoBody(input.Prompt, parent, input.SourceAssetID, input.AspectRatio, input.Mode), nil
		}, "video", videoDurationSeconds, videoResolution)
}

type bodyBuilder func(ctx context.Context, credential account.Credential, snapshot PostSnapshot, parent string) ([]byte, *Error)

func (s *Service) sendInPost(ctx context.Context, caller Caller, conversationID, label, op, jobID, sourceAssetID, parentMode string, purpose Purpose, build bodyBuilder, kind string, duration int, resolution string) (*SendResult, error) {
	owner, refusal := s.resolvePost(ctx, caller, conversationID, label)
	if refusal != nil {
		return nil, s.logged(op, conversationID, 0, refusal)
	}
	lease, refusal := s.acquirePinned(ctx, caller, owner, purpose)
	if refusal != nil {
		return nil, s.logged(op, conversationID, owner.AccountID, refusal)
	}
	defer lease.Release()
	credential := lease.Credential()
	release, busyErr := s.claimAccount(credential.ID)
	if busyErr != nil {
		return nil, s.logged(op, conversationID, credential.ID, busyErr)
	}
	defer release()

	// Every in-post send re-reads the post for free first.
	snapshot, readErr := s.readPost(ctx, credential, conversationID)
	if readErr != nil {
		return nil, s.logged(op, conversationID, credential.ID, readErr)
	}
	if !snapshot.InflightKnown || snapshot.InflightCount > 0 {
		return nil, s.logged(op, conversationID, credential.ID, notSent(CodePostInFlight, http.StatusConflict))
	}
	holder, inPost := snapshot.responseHolding(sourceAssetID)
	if !inPost {
		return nil, s.logged(op, conversationID, credential.ID, notSent(CodeItemNotInPost, http.StatusUnprocessableEntity))
	}
	parent := ""
	switch parentMode {
	case "", "head":
		parent = snapshot.HeadResponseID
	case "source":
		parent = holder.ResponseID
	default:
		if !validID.MatchString(parentMode) || !snapshot.hasResponse(parentMode) {
			return nil, s.logged(op, conversationID, credential.ID, notSent(CodeItemNotInPost, http.StatusUnprocessableEntity))
		}
		parent = parentMode
	}
	if parent == "" {
		return nil, s.logged(op, conversationID, credential.ID, notSent(CodeItemNotInPost, http.StatusUnprocessableEntity))
	}
	postRef := s.labels.Ref(conversationID)
	reserved, err := s.store.ReserveJob(ctx, caller.ClientKeyID, jobID, op, postRef)
	if err != nil {
		return nil, s.logged(op, conversationID, credential.ID, notSent(CodeInternal, http.StatusInternalServerError))
	}
	if !reserved {
		return nil, s.logged(op, conversationID, credential.ID, notSent(CodeJobIDReused, http.StatusConflict))
	}
	state := string(NotSent)
	defer func() { _ = s.store.FinishJob(context.WithoutCancel(ctx), caller.ClientKeyID, jobID, state, postRef) }()

	// The send's signature comes before any reference upload (fails closed).
	if signErr := s.signSend(ctx, credential, addResponsePath(conversationID)); signErr != nil {
		return nil, s.logged(op, conversationID, credential.ID, signErr)
	}
	if owner.OwnerUserHash == "" {
		// The read right after create failed: learn the creator from the
		// source item (made before this send) so the tripwire below works.
		if record, err := s.readAsset(ctx, credential, conversationID, sourceAssetID); err == nil && record.conversation() == conversationID {
			owner.OwnerUserHash = s.fillOwnerHash(ctx, conversationID, record.OwnerUserID)
		}
	}
	body, buildErr := build(ctx, credential, snapshot, parent)
	if buildErr != nil {
		return nil, s.logged(op, conversationID, credential.ID, buildErr)
	}
	sentAt := time.Now().UTC()
	stream, sendErr := s.send(ctx, credential, op, conversationID, addResponsePath(conversationID), body)
	if s.replyNeedsReRead(stream, sendErr) {
		// The reply was not recognised: one free re-read settles it.
		stream, sendErr = s.settleUnrecognised(ctx, credential, op, conversationID, snapshot, parent, sentAt, stream, sendErr)
	}
	if sendErr != nil {
		state = string(sendErr.Send)
		return nil, s.logged(op, conversationID, credential.ID, sendErr)
	}
	state = string(Sent)
	if len(stream.AssetIDs) == 0 {
		return nil, s.logged(op, conversationID, credential.ID, fail(CodeGenerationFailed, http.StatusBadGateway, Sent))
	}
	result := &SendResult{
		PostID: conversationID, AccountLabel: s.labels.AccountLabel(credential.ID), Kind: kind,
		ResponseID: stream.ResponseID, ParentResponseID: parent, SourceAssetID: sourceAssetID,
		AssetIDs: stream.AssetIDs, DurationS: duration, Resolution: resolution,
	}
	// Post-context check: every conversation the stream named, and the new
	// asset's own record, must be the post's.
	contextLost, accountMismatch := false, false
	for _, id := range stream.ConversationIDs {
		if id != conversationID {
			contextLost = true
		}
	}
	if record, err := s.readAsset(ctx, credential, conversationID, stream.AssetIDs[0]); err == nil {
		if record.conversation() != conversationID {
			contextLost = true
		} else if !contextLost {
			result.ContextVerified = true
		}
		// The pool-rotation tripwire: the new item must belong to the user
		// the post was created by (keyed hashes only, sec-rowan N3).
		if owner.OwnerUserHash != "" && record.OwnerUserID != "" && s.labels.UserHash(record.OwnerUserID) != owner.OwnerUserHash {
			accountMismatch = true
		}
	} else if len(stream.ConversationIDs) > 0 && !contextLost {
		result.ContextVerified = true
	}
	if accountMismatch {
		result.ContextVerified = false
		return nil, s.logged(op, conversationID, credential.ID, &Error{Code: CodeAccountMismatch, Status: http.StatusBadGateway, Send: Sent, Result: result})
	}
	if contextLost {
		result.ContextVerified = false
		return nil, s.logged(op, conversationID, credential.ID, &Error{Code: CodePostContextLost, Status: http.StatusBadGateway, Send: Sent, Result: result})
	}
	s.logOK(op, conversationID, credential.ID)
	return result, nil
}

// ListItems is one fresh free read of the post's items (list and sync).
func (s *Service) ListItems(ctx context.Context, caller Caller, conversationID, label string) (*PostSnapshot, error) {
	owner, refusal := s.resolvePost(ctx, caller, conversationID, label)
	if refusal != nil {
		return nil, s.logged("items", conversationID, 0, refusal)
	}
	lease, refusal := s.acquirePinned(ctx, caller, owner, PurposeRead)
	if refusal != nil {
		return nil, s.logged("items", conversationID, owner.AccountID, refusal)
	}
	defer lease.Release()
	snapshot, err := s.readPost(ctx, lease.Credential(), conversationID)
	if err != nil {
		return nil, s.logged("items", conversationID, owner.AccountID, err)
	}
	snapshot.PostID = conversationID
	snapshot.AccountLabel = s.labels.AccountLabel(owner.AccountID)
	s.logOK("items", conversationID, owner.AccountID)
	return &snapshot, nil
}

// ReadItem is one free read of an item's asset record, inside the post.
func (s *Service) ReadItem(ctx context.Context, caller Caller, conversationID, label, assetID string) (*ItemFacts, error) {
	facts, lease, err := s.itemOnPinned(ctx, caller, conversationID, label, assetID, "item")
	if err != nil {
		return nil, err
	}
	lease.Release()
	return facts, nil
}

// FetchMedia streams an item's media through the post's account.
func (s *Service) FetchMedia(ctx context.Context, caller Caller, conversationID, label, assetID string) (io.ReadCloser, string, error) {
	facts, lease, err := s.itemOnPinned(ctx, caller, conversationID, label, assetID, "media")
	if err != nil {
		return nil, "", err
	}
	mediaURL := "https://" + assetsHost + "/" + facts.assetKey
	if checkErr := CheckCall(s.policy(), Call{Method: http.MethodGet, Path: mediaURL}); checkErr != nil {
		lease.Release()
		return nil, "", s.logged("media", conversationID, lease.Credential().ID, checkErr.(*Error))
	}
	body, contentType, fetchErr := s.transport.FetchImagineAsset(ctx, lease.Credential(), facts.assetKey)
	if fetchErr != nil {
		lease.Release()
		return nil, "", s.logged("media", conversationID, lease.Credential().ID, mapTransportErr(fetchErr, false))
	}
	s.logOK("media", conversationID, lease.Credential().ID)
	return &leaseCloser{ReadCloser: body, lease: lease}, contentType, nil
}

type leaseCloser struct {
	io.ReadCloser
	lease Lease
	once  sync.Once
}

func (c *leaseCloser) Close() error {
	err := c.ReadCloser.Close()
	c.once.Do(c.lease.Release)
	return err
}

func (s *Service) itemOnPinned(ctx context.Context, caller Caller, conversationID, label, assetID, op string) (*ItemFacts, Lease, *Error) {
	if !validID.MatchString(assetID) {
		return nil, nil, s.logged(op, conversationID, 0, notSent(CodeItemNotInPost, http.StatusUnprocessableEntity))
	}
	owner, refusal := s.resolvePost(ctx, caller, conversationID, label)
	if refusal != nil {
		return nil, nil, s.logged(op, conversationID, 0, refusal)
	}
	lease, refusal := s.acquirePinned(ctx, caller, owner, PurposeRead)
	if refusal != nil {
		return nil, nil, s.logged(op, conversationID, owner.AccountID, refusal)
	}
	record, err := s.readAsset(ctx, lease.Credential(), conversationID, assetID)
	if err != nil {
		lease.Release()
		return nil, nil, s.logged(op, conversationID, owner.AccountID, err)
	}
	if record.conversation() != conversationID {
		lease.Release()
		return nil, nil, s.logged(op, conversationID, owner.AccountID, notSent(CodeItemNotInPost, http.StatusUnprocessableEntity))
	}
	facts := &ItemFacts{
		PostID: conversationID, AssetID: assetID, MIMEType: capText(record.MIMEType, 64), Width: record.Width, Height: record.Height,
		IsPublic: record.IsPublic, Shared: len(record.SharedWithUserIDs) > 0, InPost: true,
		AccountLabel: s.labels.AccountLabel(owner.AccountID), assetKey: record.Key,
	}
	if validID.MatchString(record.ResponseID) {
		facts.ResponseID = record.ResponseID
	}
	if owner.OwnerUserHash == "" {
		owner.OwnerUserHash = s.fillOwnerHash(ctx, conversationID, record.OwnerUserID)
	}
	if owner.OwnerUserHash != "" && record.OwnerUserID != "" {
		owned := s.labels.UserHash(record.OwnerUserID) == owner.OwnerUserHash
		facts.OwnedByPinned = &owned
	}
	if op == "item" {
		s.logOK(op, conversationID, owner.AccountID)
	}
	return facts, lease, nil
}

// Segments reads an item's cached segment map (cachedOnly: true only).
func (s *Service) Segments(ctx context.Context, caller Caller, conversationID, label, assetID string) (*Segments, error) {
	facts, lease, err := s.itemOnPinned(ctx, caller, conversationID, label, assetID, "segments")
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	body := marshal(map[string]any{"assetId": facts.AssetID, "cachedOnly": true, "maskFormat": "rle"})
	data, callErr := s.read(ctx, lease.Credential(), "segments", conversationID, http.MethodPost, pathSegment, body)
	if callErr != nil {
		return nil, s.logged("segments", conversationID, lease.Credential().ID, callErr)
	}
	cached, segments, parseErr := parseSegments(data)
	if parseErr != nil {
		return nil, s.logged("segments", conversationID, lease.Credential().ID, notSent(CodeUpstreamUnreadable, http.StatusBadGateway))
	}
	if !cached {
		return nil, s.logged("segments", conversationID, lease.Credential().ID, notSent(CodeSegmentsNotCached, http.StatusNotFound))
	}
	segments.PostID, segments.AssetID = conversationID, assetID
	s.logOK("segments", conversationID, lease.Credential().ID)
	return &segments, nil
}

// Like likes or unlikes an item. It is refused with zero calls unless
// cz.imaginePost.allowLike is true.
func (s *Service) Like(ctx context.Context, caller Caller, conversationID, label, assetID string, unlike bool) error {
	op := "like"
	if unlike {
		op = "unlike"
	}
	if !s.cfg().AllowLike {
		return s.logged(op, conversationID, 0, notSent(CodeLikeDisabled, http.StatusForbidden))
	}
	facts, lease, err := s.itemOnPinned(ctx, caller, conversationID, label, assetID, op)
	if err != nil {
		return err
	}
	defer lease.Release()
	path := pathLike
	if unlike {
		path = pathUnlike
	}
	if _, callErr := s.read(ctx, lease.Credential(), op, conversationID, http.MethodPost, path, marshal(map[string]any{"id": facts.AssetID})); callErr != nil {
		return s.logged(op, conversationID, lease.Credential().ID, callErr)
	}
	s.logOK(op, conversationID, lease.Credential().ID)
	return nil
}

// QuotaByLabel is a free quota read of an account that pins one of the
// caller's posts.
func (s *Service) QuotaByLabel(ctx context.Context, caller Caller, label string) (*Quota, error) {
	owners, err := s.store.OwnersForKey(ctx, caller.ClientKeyID)
	if err != nil {
		return nil, notSent(CodeInternal, http.StatusInternalServerError)
	}
	for _, owner := range owners {
		if s.labels.AccountLabel(owner.AccountID) != label {
			continue
		}
		lease, refusal := s.acquirePinned(ctx, caller, owner, PurposeRead)
		if refusal != nil {
			return nil, s.logged("quota", "", owner.AccountID, refusal)
		}
		defer lease.Release()
		quota, quotaErr := s.quota(ctx, lease.Credential(), "")
		if quotaErr != nil {
			return nil, s.logged("quota", "", owner.AccountID, quotaErr)
		}
		quota.AccountLabel = label
		s.logOK("quota", "", owner.AccountID)
		return &quota, nil
	}
	return nil, notSent(CodeAccountLabelMismatch, http.StatusNotFound)
}

// --- grok.com calls -------------------------------------------------------

func (s *Service) policy() Policy { return Policy{AllowLike: s.cfg().AllowLike} }

func (s *Service) readPost(ctx context.Context, credential account.Credential, conversationID string) (PostSnapshot, *Error) {
	data, err := s.read(ctx, credential, "items", conversationID, http.MethodGet, listResponsesPath(conversationID), nil)
	if err != nil {
		return PostSnapshot{}, err
	}
	snapshot, parseErr := parseResponses(data)
	if parseErr != nil {
		return PostSnapshot{}, notSent(CodeUpstreamUnreadable, http.StatusBadGateway)
	}
	return snapshot, nil
}

func (s *Service) readAsset(ctx context.Context, credential account.Credential, conversationID, assetID string) (assetRecordIn, *Error) {
	if !validID.MatchString(assetID) {
		return assetRecordIn{}, notSent(CodeItemNotInPost, http.StatusUnprocessableEntity)
	}
	data, err := s.read(ctx, credential, "asset", conversationID, http.MethodGet, "/rest/assets/"+assetID, nil)
	if err != nil {
		if err.UpstreamStatus == http.StatusNotFound {
			return assetRecordIn{}, notSent(CodeItemNotInPost, http.StatusUnprocessableEntity)
		}
		return assetRecordIn{}, err
	}
	var record assetRecordIn
	if json.Unmarshal(data, &record) != nil {
		return assetRecordIn{}, notSent(CodeUpstreamUnreadable, http.StatusBadGateway)
	}
	return record, nil
}

func (s *Service) quota(ctx context.Context, credential account.Credential, conversationID string) (Quota, *Error) {
	data, err := s.read(ctx, credential, "quota", conversationID, http.MethodPost, pathQuota, []byte(`{}`))
	if err != nil {
		return Quota{}, err
	}
	quota, parseErr := parseQuota(data)
	if parseErr != nil {
		return Quota{}, notSent(CodeUpstreamUnreadable, http.StatusBadGateway)
	}
	return quota, nil
}

const maxReadBytes = 8 << 20

// read makes one free allow-listed call and returns its body.
func (s *Service) read(ctx context.Context, credential account.Credential, op, conversationID, method, path string, body []byte) ([]byte, *Error) {
	if err := CheckCall(s.policy(), Call{Method: method, Path: path, Body: body}); err != nil {
		return nil, err.(*Error)
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg().ReadTimeout)
	defer cancel()
	response, err := s.transport.DoImagine(ctx, credential, method, path, body)
	if err != nil {
		mapped := mapTransportErr(err, false)
		s.record(ctx, op, conversationID, credential, method, path, mapped)
		if mapped.UpstreamStatus == http.StatusTooManyRequests || mapped.UpstreamStatus == http.StatusUnauthorized {
			s.pool.Report(context.WithoutCancel(ctx), credential, mapped.UpstreamStatus)
		}
		if mapped.UpstreamStatus == http.StatusNotFound && op == "items" {
			mapped = &Error{Code: CodePostNotFound, Status: http.StatusNotFound, Send: NotSent, UpstreamStatus: http.StatusNotFound, BodySHA256: mapped.BodySHA256}
		}
		return nil, mapped
	}
	defer response.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(response.Body, maxReadBytes+1))
	if readErr != nil || len(data) > maxReadBytes {
		return nil, notSent(CodeUpstreamUnreadable, http.StatusBadGateway)
	}
	return data, nil
}

// signSend obtains the Statsig signature for the send at path before any
// reference upload of that send, so nothing of a send leaves unsigned (url
// mode asks the signer for exactly this request; manual mode checks the
// value). It fails closed: on any error nothing of the send goes out.
// The send itself is still checked against the allow-list, body included,
// right before it goes out; signing calls only the signer, never grok.com's
// API.
func (s *Service) signSend(ctx context.Context, credential account.Credential, path string) *Error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg().ReadTimeout)
	defer cancel()
	if err := s.transport.SignImagine(ctx, credential, http.MethodPost, path); err != nil {
		return mapTransportErr(err, false)
	}
	return nil
}

// fillOwnerHash stores the keyed hash of a post's creator when the read
// right after create failed and left the pin without one, so the
// account_mismatch tripwire works from the first successful read on. It
// returns the hash ("" when userID is empty).
func (s *Service) fillOwnerHash(ctx context.Context, conversationID, userID string) string {
	hash := s.labels.UserHash(userID)
	if hash == "" {
		return ""
	}
	if err := s.store.FillOwnerUserHash(context.WithoutCancel(ctx), conversationID, hash); err != nil {
		s.logger.Warn("cz_imagine_owner_hash_fill_failed", "post", s.labels.Ref(conversationID))
	}
	return hash
}

// upload sends one reference image (part of a send, never makePublic).
func (s *Service) upload(ctx context.Context, credential account.Credential, image Image) (string, *Error) {
	if err := CheckCall(s.policy(), Call{Method: http.MethodPost, Path: pathUpload}); err != nil {
		return "", err.(*Error)
	}
	filename := "reference"
	if extension := imageExtension(image.MIMEType); extension != "" {
		filename += extension
	}
	id, err := s.transport.UploadImagine(ctx, credential, filename, image.MIMEType, image.Data)
	if err != nil {
		mapped := mapTransportErr(err, false)
		s.record(ctx, "upload", "", credential, http.MethodPost, pathUpload, mapped)
		return "", mapped
	}
	if !validID.MatchString(id) {
		return "", notSent(CodeUpstreamUnreadable, http.StatusBadGateway)
	}
	return id, nil
}

func imageExtension(mimeType string) string {
	switch strings.ToLower(mimeType) {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	}
	return ""
}

// send makes one generation send and reads its stream. No replay, ever.
func (s *Service) send(ctx context.Context, credential account.Credential, op, conversationID, path string, body []byte) (streamResult, *Error) {
	if err := CheckCall(s.policy(), Call{Method: http.MethodPost, Path: path, Body: body}); err != nil {
		return streamResult{}, err.(*Error)
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg().SendTimeout)
	defer cancel()
	response, err := s.transport.DoImagine(ctx, credential, http.MethodPost, path, body)
	if err != nil {
		mapped := mapTransportErr(err, true)
		s.record(ctx, op, conversationID, credential, http.MethodPost, path, mapped)
		if mapped.UpstreamStatus == http.StatusTooManyRequests || mapped.UpstreamStatus == http.StatusUnauthorized {
			s.pool.Report(context.WithoutCancel(ctx), credential, mapped.UpstreamStatus)
		}
		return streamResult{}, mapped
	}
	defer response.Body.Close()
	captureFile := s.cfg().DebugReplyShapeFile
	var capture *replyCapture
	var observe func([]byte)
	if captureFile != "" {
		capture = newReplyCapture()
		observe = capture.observe
	}
	stream, parseErr := parseSendStreamObserved(response.Body, observe)
	if capture != nil {
		s.writeReplyCapture(captureFile, op, response.StatusCode, response.Header.Get("Content-Type"), parseErr == nil, capture)
	}
	switch {
	case parseErr != nil:
		mapped := fail(CodeOutcomeUnknown, http.StatusBadGateway, Unknown)
		mapped.replyUnrecognised = true
		s.record(ctx, op, conversationID, credential, http.MethodPost, path, mapped)
		return stream, mapped
	case stream.Failed || stream.Moderated:
		mapped := &Error{Code: CodeGenerationFailed, Status: http.StatusBadGateway, Send: Sent, ProviderText: stream.ProviderText}
		s.record(ctx, op, conversationID, credential, http.MethodPost, path, mapped)
		return stream, mapped
	}
	s.record(ctx, op, conversationID, credential, http.MethodPost, path, nil)
	return stream, nil
}

// mapTransportErr turns a transport failure into a P3 error. For a send, a
// failure with no HTTP answer may have reached grok.com: outcome unknown.
func mapTransportErr(err error, isSend bool) *Error {
	var httpErr *web.ImagineHTTPError
	switch {
	case errors.Is(err, provider.ErrStatsigUnavailable):
		return notSent(CodeStatsigUnavailable, http.StatusServiceUnavailable)
	case errors.Is(err, web.ErrImagineCallRefused):
		return errCallNotAllowed()
	case errors.As(err, &httpErr):
		mapped := &Error{Code: CodeUpstreamRejected, Status: http.StatusBadGateway, Send: NotSent, UpstreamStatus: httpErr.Status, BodySHA256: httpErr.BodyPrefixSHA256}
		switch {
		case httpErr.Status == http.StatusTooManyRequests:
			mapped.Code, mapped.Status = CodeRateLimited, http.StatusTooManyRequests
		case httpErr.Status == http.StatusNotFound:
			mapped.Code, mapped.Status = CodePostNotFound, http.StatusNotFound
		case httpErr.Status >= 500 && isSend:
			mapped.Code, mapped.Send = CodeOutcomeUnknown, Unknown
		}
		return mapped
	case isSend:
		return fail(CodeOutcomeUnknown, http.StatusBadGateway, Unknown)
	default:
		return notSent(CodeUpstreamRejected, http.StatusBadGateway)
	}
}

func mapAcquireErr(err error, code string) *Error {
	mapped := notSent(code, http.StatusServiceUnavailable)
	var retry interface{ HTTPStatus() int }
	if errors.As(err, &retry) && retry.HTTPStatus() == http.StatusTooManyRequests {
		mapped.Status = http.StatusTooManyRequests
	}
	var delay interface{ RetryAfterDuration() time.Duration }
	if errors.As(err, &delay) {
		mapped.RetryAfter = delay.RetryAfterDuration()
	}
	return mapped
}

func errAccountBusy() *Error {
	return &Error{Code: CodeAccountBusy, Status: http.StatusTooManyRequests, Send: NotSent, RetryAfter: 5 * time.Second}
}

// claimAccount allows one in-flight send per account (ADR-0002 § 6 Q1).
func (s *Service) claimAccount(accountID uint64) (func(), *Error) {
	s.busyMu.Lock()
	defer s.busyMu.Unlock()
	if s.busy[accountID] {
		return nil, errAccountBusy()
	}
	s.busy[accountID] = true
	return func() {
		s.busyMu.Lock()
		delete(s.busy, accountID)
		s.busyMu.Unlock()
	}, nil
}

// --- logging and audit (sec-rowan B5) ---------------------------------------

func endpointClass(method, path string) string {
	switch {
	case path == pathCreate:
		return "conversations_new"
	case responsesPath.MatchString(strings.SplitN(path, "?", 2)[0]):
		if method == http.MethodGet {
			return "list_responses"
		}
		return "add_response"
	case assetRecord.MatchString(path):
		return "asset"
	case path == pathSegment:
		return "segment"
	case path == pathQuota:
		return "quota_info"
	case path == pathUpload:
		return "upload"
	case path == pathLike:
		return "like"
	case path == pathUnlike:
		return "unlike"
	}
	return "other"
}

func (s *Service) record(ctx context.Context, op, conversationID string, credential account.Credential, method, path string, failure *Error) {
	record := CallRecord{Op: op, AccountID: credential.ID, PostRef: s.labels.Ref(conversationID), Method: method, Endpoint: endpointClass(method, path), Code: "ok", SendState: string(Sent)}
	if failure != nil {
		record.Code, record.SendState, record.UpstreamStatus, record.BodySHA256 = failure.Code, string(failure.Send), failure.UpstreamStatus, failure.BodySHA256
	}
	if err := s.store.RecordCall(context.WithoutCancel(ctx), record); err != nil {
		s.logger.Warn("cz_imagine_audit_write_failed", "op", op)
	}
}

func (s *Service) logged(op, conversationID string, accountID uint64, failure *Error) *Error {
	s.logger.Warn("cz_imagine_call_failed", "op", op, "post", s.labels.Ref(conversationID), "account", s.labels.AccountLabel(accountID),
		"code", failure.Code, "send_state", string(failure.Send), "upstream_status", failure.UpstreamStatus)
	return failure
}

func (s *Service) logOK(op, conversationID string, accountID uint64) {
	s.logger.Info("cz_imagine_call", "op", op, "post", s.labels.Ref(conversationID), "account", s.labels.AccountLabel(accountID))
}
