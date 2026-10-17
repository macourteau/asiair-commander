package filter

import "testing"

func TestIsJunkFile(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		// Positives, straight from the artifacts design frame-lifecycle §3.2 cites.
		{"fsck recovery upper", "FSCK0000.REC", true},
		{"fsck recovery lower", "fsck0000.rec", true},
		{"fsck recovery mixed case", "FsCk0001.ReC", true},
		{"ds_store canonical case", ".DS_Store", true},
		{"ds_store lower", ".ds_store", true},
		{"appledouble prefix", "._foo", true},
		{"appledouble prefix uppercase", "._FOO.FIT", true},
		{"office lock prefix", "~$doc.docx", true},
		{"tmp suffix", "foo.tmp", true},
		{"tmp suffix uppercase", "FOO.TMP", true},
		{"tilde suffix", "bar~", true},
		{"thumbs.db", "Thumbs.db", true},
		{"ehthumbs.db", "ehthumbs.db", true},
		{"desktop.ini", "desktop.ini", true},
		{"apdisk", ".apdisk", true},
		{"timemachine donotpresent", ".com.apple.TimeMachine.donotpresent", true},
		{"media id marker", ".asiair-commander-media-id", true},
		{"media id marker uppercase", ".ASIAIR-COMMANDER-MEDIA-ID", true},
		{"rig id marker", ".asiair-commander-rig-id", true},

		// Negatives — real frame and non-junk names must survive.
		{"real light frame", "Light_NGC 7000_300.0s_Bin1_20260715-004334_0001.fit", false},
		{"real dark frame", "Dark_300.0s_Bin1_20260715-010000_0001.fit", false},
		{"real preview", "Light_NGC 7000_300.0s_Bin1_20260715-004334_0001_thn.jpg", false},
		{"plain name", "notes.txt", false},
		{"tmp in the middle is not a suffix", "tmpfile.fit", false},
		{"tilde in the middle is not a suffix", "foo~bar.fit", false},
		{"fsck without rec suffix", "fsck0000.log", false},
		{"rec suffix without fsck prefix", "backup0000.rec", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsJunkFile(tt.input); got != tt.want {
				t.Errorf("IsJunkFile(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsJunkDir(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"batch_stack_tmp lower", "batch_stack_tmp", true},
		{"batch_stack_tmp mixed case", "Batch_Stack_Tmp", true},
		{"system volume information", "System Volume Information", true},
		{"system volume information lower", "system volume information", true},
		{"synology recycle", "#recycle", true},
		{"qnap recycle", "@recycle", true},
		{"windows recycle bin", "$RECYCLE.BIN", true},
		{"spotlight", ".Spotlight-V100", true},
		{"fseventsd", ".fseventsd", true},
		{"trashes", ".Trashes", true},
		{"temporaryitems", ".TemporaryItems", true},
		{"lost+found", "lost+found", true},
		{"synology eadir", "@eaDir", true},
		{"syncthing folder marker", ".stfolder", true},
		{"syncthing versions", ".stversions", true},

		{"asiair autorun", "Autorun", false},
		{"asiair type light", "Light", false},
		{"asiair type dark", "Dark", false},
		{"target dir", "NGC 7000", false},
		{"batch_stack_tmp substring is not junk", "batch_stack_tmp2", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsJunkDir(tt.input); got != tt.want {
				t.Errorf("IsJunkDir(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestExt(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"uppercase FIT", "Light_NGC7000_0001.FIT", "fit"},
		{"lowercase fit", "Light_NGC7000_0001.fit", "fit"},
		{"uppercase JPG preview", "Light_NGC7000_0001_thn.JPG", "jpg"},
		{"lowercase jpeg", "preview.jpeg", "jpeg"},
		{"multiple dots keeps final", "archive.tar.gz", "gz"},
		{"dotfile has no extension", ".DS_Store", ""},
		{"no extension", "README", ""},
		{"empty string", "", ""},
		{"trailing dot", "foo.", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Ext(tt.input); got != tt.want {
				t.Errorf("Ext(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestExtMatches(t *testing.T) {
	tests := []struct {
		name  string
		input string
		exts  []string
		want  bool
	}{
		{"matches lowercase list", "foo.jpg", []string{"jpg", "jpeg"}, true},
		{"matches with leading dot in list", "foo.jpg", []string{".jpg", ".jpeg"}, true},
		{"case-insensitive file and list", "FOO.JPG", []string{"jpg", "jpeg"}, true},
		{"case-insensitive list entry", "foo.jpg", []string{"JPG"}, true},
		{"jpeg variant", "foo.JPEG", []string{"jpg", "jpeg"}, true},
		{"fit not in exclude list", "foo.fit", []string{"jpg", "jpeg"}, false},
		{"no extension never matches", "README", []string{"jpg"}, false},
		{"empty list never matches", "foo.jpg", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtMatches(tt.input, tt.exts); got != tt.want {
				t.Errorf("ExtMatches(%q, %v) = %v, want %v", tt.input, tt.exts, got, tt.want)
			}
		})
	}
}

func TestIsExcluded(t *testing.T) {
	excludeExts := []string{"jpg", "jpeg"}
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"jpg excluded", "Light_NGC7000_0001_thn.jpg", true},
		{"JPG excluded case-insensitive", "Light_NGC7000_0001_thn.JPG", true},
		{"jpeg excluded", "preview.jpeg", true},
		{"fit not excluded", "Light_NGC7000_0001.fit", false},
		{"FIT not excluded case-insensitive", "Light_NGC7000_0001.FIT", false},
		{"no extension not excluded", "README", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsExcluded(tt.input, excludeExts); got != tt.want {
				t.Errorf("IsExcluded(%q, %v) = %v, want %v", tt.input, excludeExts, got, tt.want)
			}
		})
	}
}

func TestPreviewStem(t *testing.T) {
	previewExts := []string{"jpg", "jpeg"}
	tests := []struct {
		name     string
		input    string
		wantStem string
		wantOK   bool
	}{
		{
			"real preview lowercase",
			"light_ngc 7000_300.0s_bin1_20260715-004334_0001_thn.jpg",
			"light_ngc 7000_300.0s_bin1_20260715-004334_0001",
			true,
		},
		{
			"real preview mixed case",
			"Light_NGC 7000_300.0s_Bin1_20260715-004334_0001_thn.JPG",
			"Light_NGC 7000_300.0s_Bin1_20260715-004334_0001",
			true,
		},
		{"jpeg extension", "frame_0001_thn.jpeg", "frame_0001", true},
		{"THN uppercase suffix", "frame_0001_THN.jpg", "frame_0001", true},
		{"missing _thn suffix", "frame_0001.jpg", "", false},
		{"not a preview extension", "frame_0001_thn.fit", "", false},
		{"no extension at all", "frame_0001_thn", "", false},
		{"bare _thn with no stem", "_thn.jpg", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotStem, gotOK := PreviewStem(tt.input, previewExts)
			if gotStem != tt.wantStem || gotOK != tt.wantOK {
				t.Errorf("PreviewStem(%q) = (%q, %v), want (%q, %v)", tt.input, gotStem, gotOK, tt.wantStem, tt.wantOK)
			}
		})
	}
}

func TestIsSidecarOf(t *testing.T) {
	previewExts := []string{"jpg", "jpeg"}
	tests := []struct {
		name        string
		previewName string
		frameName   string
		want        bool
	}{
		{
			"design's real example",
			"Light_NGC 7000_300.0s_Bin1_20260715-004334_0001_thn.jpg",
			"Light_NGC 7000_300.0s_Bin1_20260715-004334_0001.fit",
			true,
		},
		{
			"uppercase extensions on both sides",
			"Light_NGC 7000_300.0s_Bin1_20260715-004334_0001_thn.JPG",
			"Light_NGC 7000_300.0s_Bin1_20260715-004334_0001.FIT",
			true,
		},
		{
			"different stem",
			"Light_NGC 7000_300.0s_Bin1_20260715-004334_0002_thn.jpg",
			"Light_NGC 7000_300.0s_Bin1_20260715-004334_0001.fit",
			false,
		},
		{
			"non-image preview extension",
			"Light_NGC 7000_300.0s_Bin1_20260715-004334_0001_thn.txt",
			"Light_NGC 7000_300.0s_Bin1_20260715-004334_0001.fit",
			false,
		},
		{
			// The design explicitly rejects a naive same-stem compare: a
			// same-named JPEG lacking the ASIAIR "_thn" convention is not
			// treated as a sidecar, even though its stem matches exactly.
			"same stem but missing _thn suffix is not a sidecar",
			"Light_NGC 7000_300.0s_Bin1_20260715-004334_0001.jpg",
			"Light_NGC 7000_300.0s_Bin1_20260715-004334_0001.fit",
			false,
		},
		{
			"frame with multiple dots in name",
			"archive.tar_thn.jpg",
			"archive.tar.fit",
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSidecarOf(tt.previewName, tt.frameName, previewExts); got != tt.want {
				t.Errorf("IsSidecarOf(%q, %q) = %v, want %v", tt.previewName, tt.frameName, got, tt.want)
			}
		})
	}
}
