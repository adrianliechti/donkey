package config

import "strings"

// SkippedUploadDir names directories that never belong in a build context:
// build inputs come from sources, not local artifacts or VCS internals. The
// CLI skips them while packing; the server skips them when unpacking browser
// uploads (which arrive as-is).
func SkippedUploadDir(name string) bool {
	switch name {
	case ".git", "node_modules", ".venv", "venv", "__pycache__", ".idea", ".vscode",
		// Finder's "Compress" writes this sibling tree into every zip; left in
		// place it also defeats the single-root unwrap of browser uploads.
		"__MACOSX":
		return true
	}
	return false
}

// SensitiveUploadFile is a basename guard against key material reaching a
// build context. It stays narrow on purpose: overly broad patterns (like
// every file starting with "id_") would silently drop legitimate source
// files from the build.
func SensitiveUploadFile(name string) bool {
	lower := strings.ToLower(name)
	if lower == ".ds_store" {
		return true
	}
	if strings.HasPrefix(lower, ".env") {
		return true
	}
	for _, ext := range []string{".pem", ".key", ".p12", ".pfx"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	// SSH keys by their conventional names, not everything id_-prefixed.
	for _, ssh := range []string{"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519"} {
		if lower == ssh || lower == ssh+".pub" {
			return true
		}
	}
	return false
}
