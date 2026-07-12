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
// as root) leaves them root-owned. This includes the intermediate ancestors
// MkdirAll must create to reach dir (e.g. the box user's ~/.claude on the way to
// ~/.claude/skills), so the box user can also write siblings like ~/.claude/
// downloads. dir must be non-empty — an empty dir is a
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
// @testcase TestInstallSkillsChownsCreatedAncestors chowns the ~/.claude parent created on the way to the skills dir.
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
	// Create dir up front, chowning every ancestor MkdirAll has to create on the
	// way (e.g. the box user's ~/.claude parent of ~/.claude/skills). The walk
	// below only ever chowns dir and entries under it, so without this the
	// implicitly-created parents stay root-owned and the box user cannot write
	// anything else beneath them (~/.claude/downloads, settings, ...).
	if err := mkdirAllChown(dir, uid, gid); err != nil {
		return err
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

// mkdirAllChown creates dir and every missing ancestor like os.MkdirAll, then
// chowns exactly the directories it created to uid/gid (skipping the chown when
// both are zero, per chownSkill). os.MkdirAll silently creates intermediate
// ancestors owned by the caller — here the guest running as root — so creating
// ~/.claude/skills leaves the ~/.claude parent root-owned, which blocks the
// unprivileged box user from writing anything else under it. Only the ancestors
// that did not already exist are chowned, so a pre-existing home dir is left
// untouched.
//
// @arg dir The directory to create (with its missing ancestors).
// @arg uid The owner uid applied to created directories; with gid 0 the chown is skipped.
// @arg gid The owner gid applied to created directories; with uid 0 the chown is skipped.
// @error error if a directory cannot be stat'd, created, or chowned.
//
// @testcase TestInstallSkillsChownsCreatedAncestors chowns the ~/.claude parent created on the way to the skills dir.
func mkdirAllChown(dir string, uid, gid int) error {
	// Walk up from dir collecting the ancestors that don't exist yet; MkdirAll
	// will create exactly these, so they are exactly the ones to chown.
	var created []string
	for p := filepath.Clean(dir); ; {
		if _, err := os.Stat(p); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("stat %s: %w", p, err)
		}
		created = append(created, p)
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating skills dir %s: %w", dir, err)
	}
	for _, p := range created {
		if err := chownSkill(p, uid, gid); err != nil {
			return err
		}
	}
	return nil
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
