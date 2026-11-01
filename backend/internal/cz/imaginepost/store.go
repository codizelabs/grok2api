package imaginepost

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// PostOwner pins a post (grok.com conversation) to the account that created
// it and the client key that asked (ADR-0002 § 1). The row never changes
// after insert. OwnerUserHash is a keyed hash of the account's grok.com user
// id (sec-rowan N3), never the raw id.
type PostOwner struct {
	ConversationID string `gorm:"primaryKey;size:128"`
	AccountID      uint64 `gorm:"not null;index"`
	ClientKeyID    uint64 `gorm:"not null;index"`
	OwnerUserHash  string `gorm:"size:64"`
	CreatedAt      time.Time
}

// TableName is fixed by ADR-0002 § 1.
func (PostOwner) TableName() string { return "cz_imagine_post_owner" }

// SendKey is a used job_id: one send per job, ever (contract F5).
type SendKey struct {
	ClientKeyID uint64 `gorm:"primaryKey"`
	JobID       string `gorm:"primaryKey;size:128"`
	Op          string `gorm:"size:16;not null"`
	State       string `gorm:"size:24;not null"`
	PostRef     string `gorm:"size:16"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// TableName keeps P3's tables under the cz_ prefix.
func (SendKey) TableName() string { return "cz_imagine_send_key" }

// CallRecord is P3's audit row for one grok.com call: a body hash and codes,
// never a body, prompt, title, ownerUserId, URL or raw id (sec-rowan B5).
type CallRecord struct {
	ID             uint64    `gorm:"primaryKey;autoIncrement"`
	At             time.Time `gorm:"index;not null"`
	Op             string    `gorm:"size:16;not null"`
	AccountID      uint64
	PostRef        string `gorm:"size:16"`
	Method         string `gorm:"size:8"`
	Endpoint       string `gorm:"size:32"`
	UpstreamStatus int
	Code           string `gorm:"size:48"`
	SendState      string `gorm:"size:16"`
	BodySHA256     string `gorm:"size:80"`
}

// TableName keeps P3's tables under the cz_ prefix.
func (CallRecord) TableName() string { return "cz_imagine_call" }

// callRetention is how long call records live (audit.retentionDays: 1).
const callRetention = 24 * time.Hour

// Store is P3's persistence.
type Store struct {
	db *gorm.DB
}

// NewStore wraps a GORM handle.
func NewStore(db *gorm.DB) *Store { return &Store{db: db} }

// Migrate creates P3's tables. It is additive and touches no upstream table.
func (s *Store) Migrate(ctx context.Context) error {
	return s.db.WithContext(ctx).AutoMigrate(&PostOwner{}, &SendKey{}, &CallRecord{})
}

var errNotFound = errors.New("not found")

// Owner returns a post's pin.
func (s *Store) Owner(ctx context.Context, conversationID string) (PostOwner, error) {
	var owner PostOwner
	err := s.db.WithContext(ctx).Where("conversation_id = ?", conversationID).Take(&owner).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return PostOwner{}, errNotFound
	}
	return owner, err
}

// OwnersForKey lists the pins a client key made.
func (s *Store) OwnersForKey(ctx context.Context, clientKeyID uint64) ([]PostOwner, error) {
	var owners []PostOwner
	err := s.db.WithContext(ctx).Where("client_key_id = ?", clientKeyID).Find(&owners).Error
	return owners, err
}

// PutOwner records a new pin; an existing pin is never overwritten.
func (s *Store) PutOwner(ctx context.Context, owner PostOwner) error {
	owner.CreatedAt = time.Now().UTC()
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&owner).Error
}

// ReserveJob claims a job_id. It reports false when the id was already used.
func (s *Store) ReserveJob(ctx context.Context, clientKeyID uint64, jobID, op, postRef string) (bool, error) {
	now := time.Now().UTC()
	result := s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&SendKey{
		ClientKeyID: clientKeyID, JobID: jobID, Op: op, State: "reserved", PostRef: postRef, CreatedAt: now, UpdatedAt: now,
	})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// FinishJob records how a reserved job ended. The key stays used.
func (s *Store) FinishJob(ctx context.Context, clientKeyID uint64, jobID, state, postRef string) error {
	updates := map[string]any{"state": state, "updated_at": time.Now().UTC()}
	if postRef != "" {
		updates["post_ref"] = postRef
	}
	return s.db.WithContext(ctx).Model(&SendKey{}).Where("client_key_id = ? AND job_id = ?", clientKeyID, jobID).Updates(updates).Error
}

// RecordCall stores one audit row and prunes rows past retention.
func (s *Store) RecordCall(ctx context.Context, record CallRecord) error {
	record.At = time.Now().UTC()
	db := s.db.WithContext(ctx)
	if err := db.Create(&record).Error; err != nil {
		return err
	}
	return db.Where("at < ?", record.At.Add(-callRetention)).Delete(&CallRecord{}).Error
}

// Calls lists the audit rows, newest last (tests and diagnostics).
func (s *Store) Calls(ctx context.Context) ([]CallRecord, error) {
	var records []CallRecord
	err := s.db.WithContext(ctx).Order("id").Find(&records).Error
	return records, err
}
