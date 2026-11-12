package docker

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/adrianliechti/donkey/internal/config"
)

func (s state) migrateLegacyRoles() error {
	for _, folder := range []string{"routes", "spaces", "platform"} {
		err := filepath.WalkDir(s.path(folder), func(path string, entry fs.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			name, ok := recordName(entry.Name())
			if !ok || (folder != "routes" && name != "config") {
				return nil
			}
			base, _ := recordName(path)
			if strings.HasSuffix(path, legacyRecordExt) {
				if _, err := os.Stat(base + recordExt); err == nil {
					return nil
				}
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fields := []string{"admins", "developers", "viewers"}
			if folder == "routes" {
				fields = []string{"access"}
			}
			if folder == "platform" {
				fields = []string{"admins"}
			}
			raw, removed, err := config.RemoveLegacyRoles(data, fields...)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if raw == nil {
				return nil
			}
			var record map[string]any
			if err := config.DecodeYAML(raw, &record); err != nil {
				return err
			}
			rel, err := filepath.Rel(s.dir, base)
			if err != nil {
				return err
			}
			if err := s.writeRecord(record, rel); err != nil {
				return err
			}
			if len(removed) > 0 {
				log.Printf("WARNING: removed unsupported legacy role grants from %s: %q; assign users/groups explicitly to restore access", path, removed)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}
