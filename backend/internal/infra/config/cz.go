package config

// CZConfig holds CodizeLabs fork settings (patch P3, ADR-0002). Every switch
// defaults to off.
type CZConfig struct {
	ImaginePost CZImaginePostConfig `yaml:"imaginePost"`
}

// CZImaginePostConfig configures internal/cz/imaginepost.
type CZImaginePostConfig struct {
	// AllowLike opens like/unlike on P3's allow-list. Default false: like is a
	// visible account write that needs an owner approval (ADR-0002 § 6 Q4).
	AllowLike bool `yaml:"allowLike"`
}
