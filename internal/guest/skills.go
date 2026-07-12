package guest

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// DefaultSkillsDir is where the guest installs the embedded agent skills by
// default: under the box user's Claude home, so the box's agent discovers them.
// It matches the unprivileged `agent` account the Firecracker base provisions
// (home /home/agent); a box using a different account overrides it with the
// guest's --skills-dir flag.
const DefaultSkillsDir = "/home/agent/.claude/skills"

// skillsFS holds the agent skill tree shipped inside the guest binary. The
// `all:` prefix keeps files the default embed pattern would skip (dotfiles and
// underscore-prefixed names); the skill payload is plain SKILL.md today but a
// skill may grow supporting files.
//
//go:embed all:skills
var skillsFS embed.FS

// InstallSkills writes the guest's embedded agent skills into dir (typically
// DefaultSkillsDir) so the box's agent learns how to drive the box API. It is
// idempotent: existing files are overwritten, so a guest upgrade refreshes the
// skills in place. Every directory it creates and every file it writes is
// chowned to uid/gid when they are non-zero, so the unprivileged box user the
// agent runs as can read (and manage) them; a zero uid/gid (the guest running
// as root) leaves them root-owned. dir must be non-empty — an empty dir is a
// caller signal to skip installation and is reported as an error here so the
// caller can decide.
//
// @arg dir The directory to install the skill tree into; must be non-empty.
// @arg uid The owner uid to apply to created files and dirs; 0 leaves them root-owned.
// @arg gid The owner gid to apply to created files and dirs; 0 leaves them root-owned.
// @error error if dir is empty, the embedded tree cannot be read, or a file or directory cannot be written or chowned.
//
// @testcase TestInstallSkillsWritesTree installs the embedded skills and finds SKILL.md at the expected path.
// @testcase TestInstallSkillsChownsToBoxUser applies uid/gid to the created files and dirs.
// @testcase TestInstallSkillsOverwrites refreshes an existing skill file on a second install.
// @testcase TestInstallSkillsRejectsEmptyDir errors when dir is empty.
func InstallSkills(dir string, uid, gid int) error {
	if dir == "" {
		return fmt.Errorf("skills dir is empty")
	}
	root, err := fs.Sub(skillsFS, "skills")
	if err != nil {
		return fmt.Errorf("opening embedded skills: %w", err)
	}
	return fs.WalkDir(root, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dest := filepath.Join(dir, path)
		if d.IsDir() {
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return fmt.Errorf("creating skills dir %s: %w", dest, err)
			}
			return chownSkill(dest, uid, gid)
		}
		data, err := fs.ReadFile(root, path)
		if err != nil {
			return fmt.Errorf("reading embedded skill %s: %w", path, err)
		}
		if err := os.WriteFile(dest, data, 0o644); err != nil {
			return fmt.Errorf("writing skill %s: %w", dest, err)
		}
		return chownSkill(dest, uid, gid)
	})
}

// chownSkill applies uid/gid to an installed skill path, skipping the chown when
// both are zero (the guest runs as root and leaving the file root-owned is
// correct). A world-readable skill stays readable by the box user regardless, so
// this only matters when a non-root box user must also manage the tree.
//
// @arg path The installed file or directory to chown.
// @arg uid The owner uid; with gid 0 it skips the chown.
// @arg gid The owner gid; with uid 0 it skips the chown.
// @error error if the chown fails.
//
// @testcase TestInstallSkillsChownsToBoxUser drives chownSkill through InstallSkills.
func chownSkill(path string, uid, gid int) error {
	if uid == 0 && gid == 0 {
		return nil
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return fmt.Errorf("chown skill %s: %w", path, err)
	}
	return nil
}
