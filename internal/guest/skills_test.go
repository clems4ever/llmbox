package guest

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestInstallSkillsWritesTree installs the embedded skills into a fresh dir and
// checks the box-API skill lands at its expected path with real content.
func TestInstallSkillsWritesTree(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills")
	if err := InstallSkills(dir, 0, 0); err != nil {
		t.Fatalf("InstallSkills: %v", err)
	}
	skill := filepath.Join(dir, "llmbox-ports", "SKILL.md")
	data, err := os.ReadFile(skill)
	if err != nil {
		t.Fatalf("reading installed skill: %v", err)
	}
	// The skill must name the socket the box API is reached on and its frontmatter
	// name, so the agent both discovers it and knows where to talk.
	for _, want := range []string{"name: llmbox-ports", "/run/llmbox/boxapi.sock", "open_port"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("installed skill missing %q", want)
		}
	}
}

// TestInstallSkillsOverwrites checks a second install refreshes an existing
// skill file rather than failing on the pre-existing tree.
func TestInstallSkillsOverwrites(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills")
	if err := InstallSkills(dir, 0, 0); err != nil {
		t.Fatalf("first InstallSkills: %v", err)
	}
	skill := filepath.Join(dir, "llmbox-ports", "SKILL.md")
	if err := os.WriteFile(skill, []byte("stale"), 0o644); err != nil {
		t.Fatalf("staling skill: %v", err)
	}
	if err := InstallSkills(dir, 0, 0); err != nil {
		t.Fatalf("second InstallSkills: %v", err)
	}
	data, err := os.ReadFile(skill)
	if err != nil {
		t.Fatalf("reading refreshed skill: %v", err)
	}
	if string(data) == "stale" {
		t.Error("second InstallSkills did not overwrite the stale skill")
	}
}

// TestInstallSkillsChownsToBoxUser checks the installed files and dirs are
// chowned to the given uid/gid so the unprivileged box user can read them. It
// needs root (to chown to another owner); skipped otherwise.
func TestInstallSkillsChownsToBoxUser(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("chown to another owner requires root")
	}
	const uid, gid = 12345, 12345
	dir := filepath.Join(t.TempDir(), "skills")
	if err := InstallSkills(dir, uid, gid); err != nil {
		t.Fatalf("InstallSkills: %v", err)
	}
	skill := filepath.Join(dir, "llmbox-ports", "SKILL.md")
	fi, err := os.Stat(skill)
	if err != nil {
		t.Fatalf("stat skill: %v", err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	if st.Uid != uid || st.Gid != gid {
		t.Fatalf("skill owned by %d:%d, want %d:%d", st.Uid, st.Gid, uid, gid)
	}
	// The created skill directory must be chowned too, so the box user can
	// traverse and manage it.
	di, err := os.Stat(filepath.Join(dir, "llmbox-ports"))
	if err != nil {
		t.Fatalf("stat skill dir: %v", err)
	}
	dst := di.Sys().(*syscall.Stat_t)
	if dst.Uid != uid || dst.Gid != gid {
		t.Fatalf("skill dir owned by %d:%d, want %d:%d", dst.Uid, dst.Gid, uid, gid)
	}
}

// TestInstallSkillsRejectsEmptyDir checks an empty dir is reported as an error
// so the caller can treat it as "installation disabled".
func TestInstallSkillsRejectsEmptyDir(t *testing.T) {
	if err := InstallSkills("", 0, 0); err == nil {
		t.Fatal("InstallSkills(\"\") = nil, want an error")
	}
}
