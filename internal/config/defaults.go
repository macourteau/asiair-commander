package config

import (
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
)

// dur is a small constructor for model.Duration from a time.Duration.
func dur(d time.Duration) model.Duration { return model.Duration(d) }

// Defaults returns a fully-populated config carrying the shipped defaults (design
// architecture §7). The setup wizard uses it as a starting point. No secrets are
// set (PasswordHash/SessionSecret empty) so a freshly-defaulted config still
// reads as a first run. Defaults passes Validate (with a nil VolumeResolver) with
// no errors. Preview defaults. Shared with the v1->v2 migration so an upgraded
// install and a fresh install agree.
const (
	defaultPreviewsEnabled      = true
	defaultPreviewConcurrency   = 2
	defaultPreviewCacheMaxBytes = 2 * 1024 * 1024 * 1024 // 2 GiB
)

// Plate-solve defaults. Shared with the v2->v3 migration so an upgraded install
// and a fresh install agree. Default-on means an upgraded install auto-downloads
// the d05 catalog after startup; radius 10° solved a ~5° field in 0.26 s.
const (
	defaultPlateSolveEnabled   = true
	defaultPlateSolveRadiusDeg = 10
)

// defaultReconcileSettle is the shipped settle window before a device original may
// be deleted after its NAS copy is observed missing. Shared with the v3->v4
// migration so an upgraded install (whose stored config predates the field, so it
// would otherwise load as zero and fail validation) and a fresh install agree.
const defaultReconcileSettle = 24 * time.Hour

// defaultMosaicTemplate is the shipped mosaic layout. NormalizeTemplates fills a
// blank templates.mosaic with it, so an install that predates the field is
// pre-filled the moment the operator reveals it rather than being rewritten by a
// migration — the default names <OTA> and <SESSION>, and writing it into an
// install that leaves those blank would invalidate a config that is otherwise
// valid.
//
// It spells the leaf out as <TARGET_BASE>_<PANEL> rather than <TARGET> — the two
// render byte-identical directories — so every segment of the mosaic template is
// visibly panel-aware and no one has to reason about what <TARGET> means here.
const defaultMosaicTemplate = "<TARGET_BASE>/<OTA>/<SESSION>/<TARGET_BASE>_<PANEL>/NIGHT_<n>"

func Defaults() *model.Config {
	return &model.Config{
		SchemaVersion: model.CurrentSchemaVersion,
		WebPort:       8686,

		StripTargetSpaces: true,
		// Roots are intentionally empty: the operator picks them in the setup
		// wizard, which pre-fills defaults on Windows and shows examples
		// elsewhere, in the form where the operator sees and can change them.
		// Baking real paths in here would file into and try to create a drive
		// that may not exist on this machine.
		Roots:                   model.Roots{},
		ExcludeExtensions:       []string{"jpg", "jpeg"},
		DeleteSidecarExtensions: []string{"jpg", "jpeg"},
		DiskReserveBytes:        5 * 1024 * 1024 * 1024, // 5 GiB
		MaxConcurrentTransfers:  3,

		PreviewsEnabled:      defaultPreviewsEnabled,
		PreviewConcurrency:   defaultPreviewConcurrency,
		PreviewCacheMaxBytes: defaultPreviewCacheMaxBytes,

		PlateSolveEnabled:   defaultPlateSolveEnabled,
		PlateSolveRadiusDeg: defaultPlateSolveRadiusDeg,

		// Off by default, and stated rather than left to the zero value: mosaic
		// recognition is a parse of a producer naming convention, so a target of
		// the operator's own shaped like "<name>_1-1" would start filing
		// somewhere else. Opting in is the operator's decision, not a default.
		MosaicDetection: false,

		Defaults: model.RigDefaults{
			PollInterval:    dur(45 * time.Second),
			RetryInterval:   dur(5 * time.Minute),
			SettleCycles:    1,
			MinFileAge:      dur(300 * time.Second),
			Grace:           dur(1 * time.Hour),
			ReconcileSettle: dur(defaultReconcileSettle),
			FailedRetry: model.FailedRetry{
				Base:        dur(2 * time.Minute),
				Factor:      2,
				Max:         dur(1 * time.Hour),
				MaxAttempts: 6,
			},
			UnreachableAlertAfter: dur(15 * time.Minute),
		},

		Web: model.WebSettings{
			BcryptCost:         12,
			SessionIdleTimeout: dur(7 * 24 * time.Hour),
			SessionMaxAge:      dur(30 * 24 * time.Hour),
			LoginBackoffBase:   dur(1 * time.Second),
			LoginBackoffCap:    dur(30 * time.Second),
			LoginGlobalCap:     20,
			AllowedHosts:       []string{"localhost"},
			MetricsExposeLAN:   false,
		},
		SMB: model.SMBSettings{
			RemoveRetryBudget: 10,
		},
		DateFormat: "2006-01-02",

		AliasTable: map[string]string{
			"light":      "LIGHT",
			"dark":       "DARK",
			"flat":       "FLAT",
			"bias":       "BIAS",
			"dark frame": "DARK",
			"bias frame": "BIAS",
		},
		SensorMap: map[string]string{
			"ZWO ASI6200MM Pro": "6200MM",
			"ZWO ASI2600MM Pro": "2600MM",
			"ZWO ASI294MC Pro":  "294MC",
		},
		Templates: model.Templates{
			Light:  "<TARGET>/<OTA>/<SESSION>/NIGHT_<n>",
			Mosaic: defaultMosaicTemplate,
			Cal: map[model.FrameType]string{
				model.TypeDark: "<SENSOR>/DARK/<GAIN>/<BINNING>/<SET-TEMP>/<EXPOSURE>/<DATE>",
				model.TypeFlat: "<SENSOR>/FLAT/<OTA>/<GAIN>/<BINNING>/<DATE>",
				model.TypeBias: "<SENSOR>/BIAS/<GAIN>/<BINNING>/<SET-TEMP>/<DATE>",
			},
		},
	}
}
