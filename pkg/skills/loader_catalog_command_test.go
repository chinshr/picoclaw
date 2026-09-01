package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeSkill drops a SKILL.md with the given frontmatter into a workspace laid
// out the way picoclaw expects (<workspace>/skills/<name>/SKILL.md) and returns
// the workspace root, ready for NewSkillsLoader.
func writeSkill(t *testing.T, name, frontmatter, body string) string {
	t.Helper()
	workspace := t.TempDir()
	dir := filepath.Join(workspace, "skills", name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	content := "---\n" + frontmatter + "\n---\n\n" + body
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644))
	return workspace
}

func TestSkillCommandSurfacedInCatalog(t *testing.T) {
	root := writeSkill(t, "vehicles",
		"name: vehicles\ndescription: Count cars on the street.\ncommand: library skills vehicles exec vehicle-count \"<window>\"",
		"# Vehicles\n")

	sl := NewSkillsLoader(root, "", "")
	skills := sl.ListSkills()
	require.Len(t, skills, 1)
	assert.Equal(t, `library skills vehicles exec vehicle-count "<window>"`, skills[0].Command)

	// escapeXML covers &, < and > only. A bare double quote is legal in element
	// text, so the command's quotes survive verbatim — which matters, because the
	// model is meant to copy this line into a shell.
	summary := sl.BuildSkillsSummary()
	assert.Contains(t, summary, `<command>library skills vehicles exec vehicle-count "&lt;window&gt;"</command>`)
	assert.Contains(t, summary, "<name>vehicles</name>")
}

func TestSkillWithoutCommandEmitsNoCommandTag(t *testing.T) {
	root := writeSkill(t, "stars", "name: stars\ndescription: One stargazing tip.", "# Stars\n")

	sl := NewSkillsLoader(root, "", "")
	skills := sl.ListSkills()
	require.Len(t, skills, 1)
	assert.Empty(t, skills[0].Command)
	assert.NotContains(t, sl.BuildSkillsSummary(), "<command>")
}

// The regression this whole change exists for: workspace `inventory` carried a
// 1,569-character description and ListSkills dropped it, so the largest skill on
// the shelf was absent from the catalog with only a slog.Warn to say so.
func TestOversizeDescriptionIsTruncatedNotDropped(t *testing.T) {
	long := strings.Repeat("a", MaxDescriptionLength+500)
	root := writeSkill(t, "inventory", "name: inventory\ndescription: "+long, "# Inventory\n")

	sl := NewSkillsLoader(root, "", "")
	skills := sl.ListSkills()

	require.Len(t, skills, 1, "an over-long description must not remove the skill from the catalog")
	assert.Equal(t, "inventory", skills[0].Name)
	assert.Len(t, skills[0].Description, MaxDescriptionLength)
	assert.Contains(t, sl.BuildSkillsSummary(), "<name>inventory</name>")
}

// Truncation must not split a multi-byte rune — the real descriptions are full of
// em-dashes, which is exactly where a byte-wise cut would produce mojibake.
func TestTruncationRespectsRuneBoundaries(t *testing.T) {
	// Land an em-dash (3 bytes) astride the cap.
	head := strings.Repeat("a", MaxDescriptionLength-1)
	desc := head + "—" + strings.Repeat("b", 100)
	root := writeSkill(t, "wordy", "name: wordy\ndescription: "+desc, "# Wordy\n")

	sl := NewSkillsLoader(root, "", "")
	skills := sl.ListSkills()
	require.Len(t, skills, 1)

	got := skills[0].Description
	assert.True(t, len(got) <= MaxDescriptionLength)
	assert.Equal(t, head, got, "the partial em-dash must be dropped whole")
	assert.True(t, utf8.ValidString(got))
}

func TestOverlongCommandIsDroppedButSkillSurvives(t *testing.T) {
	root := writeSkill(t, "chatty",
		"name: chatty\ndescription: A skill.\ncommand: "+strings.Repeat("x", MaxCommandLength+1),
		"# Chatty\n")

	sl := NewSkillsLoader(root, "", "")
	skills := sl.ListSkills()
	require.Len(t, skills, 1)
	assert.Empty(t, skills[0].Command, "an unusable command must not take the skill down with it")
	assert.NotContains(t, sl.BuildSkillsSummary(), "<command>")
}

func TestClampToBytesShortStringUntouched(t *testing.T) {
	assert.Equal(t, "short", clampToBytes("short", MaxDescriptionLength))
	assert.Equal(t, "", clampToBytes("", 10))
	assert.Equal(t, "ab", clampToBytes("ab—cd", 4), "cuts back to the rune boundary")
}

// Finding 14a: a plain YAML scalar may not contain ": ". Four shipped skills had
// one, so yaml.Unmarshal failed on the document and every frontmatter field was
// discarded — silently, until now. The catalog then showed the body's first
// paragraph, which is a purpose statement where a routing statement belongs.
func TestUnquotedColonVoidsFrontmatterAndFallsBackToBody(t *testing.T) {
	root := writeSkill(t, "visitors",
		"name: visitors\ndescription: Count door visits with one command: visitor-count.\ncommand: 'library skills visitors exec visitor-count'",
		"# Library Visitors\n\nAnswer \"how busy was the library?\" from the database.\n")

	sl := NewSkillsLoader(root, "", "")
	skills := sl.ListSkills()
	require.Len(t, skills, 1)

	assert.Equal(t, "visitors", skills[0].Name, "name falls back to the directory")
	assert.Contains(t, skills[0].Description, "how busy was the library",
		"the body paragraph is what the model would see")
	assert.NotContains(t, skills[0].Description, "visitor-count")
	assert.Empty(t, skills[0].Command, "the command is lost with the rest of the frontmatter")
}

// The fix on the workspace side: a folded block scalar carries the same prose,
// colons and all.
func TestFoldedBlockScalarSurvivesColons(t *testing.T) {
	root := writeSkill(t, "visitors",
		"name: visitors\ndescription: >-\n  Count door visits with one command: visitor-count.\n"+
			"command: 'library skills visitors exec visitor-count'",
		"# Library Visitors\n\nAnswer \"how busy was the library?\" from the database.\n")

	sl := NewSkillsLoader(root, "", "")
	skills := sl.ListSkills()
	require.Len(t, skills, 1)

	assert.Equal(t, "Count door visits with one command: visitor-count.", skills[0].Description)
	assert.Equal(t, "library skills visitors exec visitor-count", skills[0].Command)
}

