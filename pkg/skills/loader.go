package skills

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/gomarkdown/markdown"
	"github.com/gomarkdown/markdown/ast"
	"github.com/gomarkdown/markdown/parser"
	"gopkg.in/yaml.v3"

	"github.com/sipeed/picoclaw/pkg/logger"
)

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9]+(-[a-zA-Z0-9]+)*$`)

const (
	MaxNameLength        = 64
	MaxDescriptionLength = 1024
	MaxCommandLength     = 256
)

type SkillMetadata struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Confirm marks a long-running, real-world action skill (e.g. watering the
	// dog) that must be confirmed with the visitor before it runs. Surfaced to
	// the model in the skill catalog; the prompt rule turns it into a
	// "say what you'll do, wait for a yes" step. Set via frontmatter `confirm: true`.
	Confirm bool `json:"confirm"`
	// Command is the skill's canonical invocation, surfaced in the catalog so the
	// model can run the common case without spending a round trip reading
	// SKILL.md first. Set via frontmatter `command:`. Only set it when running
	// that command blind is genuinely safe — it is a promise that the
	// description carries everything the hot path needs.
	Command string `json:"command"`
}

type SkillInfo struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Source      string `json:"source"`
	Description string `json:"description"`
	Confirm     bool   `json:"confirm"`
	Command     string `json:"command"`
}

func (info SkillInfo) validate() error {
	var errs error
	if info.Name == "" {
		errs = errors.Join(errs, errors.New("name is required"))
	} else {
		if err := ValidateSkillName(info.Name); err != nil {
			errs = errors.Join(errs, err)
		}
	}

	if info.Description == "" {
		errs = errors.Join(errs, errors.New("description is required"))
	} else if len(info.Description) > MaxDescriptionLength {
		errs = errors.Join(errs, fmt.Errorf("description exceeds %d character", MaxDescriptionLength))
	}
	if len(info.Command) > MaxCommandLength {
		errs = errors.Join(errs, fmt.Errorf("command exceeds %d character", MaxCommandLength))
	}
	return errs
}

// clampToBytes cuts s to at most max bytes without splitting a rune. Used so an
// over-long description costs the model a clipped sentence instead of costing it
// the whole skill: ListSkills used to drop the entry, which silently removed the
// skill from the catalog (workspace `inventory` was invisible this way).
func clampToBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

type SkillsLoader struct {
	workspace       string
	workspaceSkills string // workspace skills (project-level)
	globalSkills    string // global skills (~/.picoclaw/skills)
	builtinSkills   string // builtin skills
}

// SkillRoots returns all unique skill root directories used by this loader.
// The order follows resolution priority: workspace > global > builtin.
func (sl *SkillsLoader) SkillRoots() []string {
	roots := []string{sl.workspaceSkills, sl.globalSkills, sl.builtinSkills}
	seen := make(map[string]struct{}, len(roots))
	out := make([]string, 0, len(roots))

	for _, root := range roots {
		trimmed := strings.TrimSpace(root)
		if trimmed == "" {
			continue
		}
		clean := filepath.Clean(trimmed)
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}

	return out
}

func NewSkillsLoader(workspace string, globalSkills string, builtinSkills string) *SkillsLoader {
	return &SkillsLoader{
		workspace:       workspace,
		workspaceSkills: filepath.Join(workspace, "skills"),
		globalSkills:    globalSkills, // ~/.picoclaw/skills
		builtinSkills:   builtinSkills,
	}
}

func (sl *SkillsLoader) ListSkills() []SkillInfo {
	skills := make([]SkillInfo, 0)
	seen := make(map[string]bool)

	addSkills := func(dir, source string) {
		if dir == "" {
			return
		}
		dirs, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, d := range dirs {
			if !d.IsDir() {
				continue
			}
			skillFile := filepath.Join(dir, d.Name(), "SKILL.md")
			if _, err := os.Stat(skillFile); err != nil {
				continue
			}
			info := SkillInfo{
				Name:   d.Name(),
				Path:   skillFile,
				Source: source,
			}
			metadata := sl.getSkillMetadata(skillFile)
			if metadata != nil {
				info.Description = metadata.Description
				info.Name = metadata.Name
				info.Confirm = metadata.Confirm
				info.Command = metadata.Command
			}
			// Truncate rather than reject: a clipped description still routes the
			// skill, a dropped skill does not exist as far as the model is concerned.
			if len(info.Description) > MaxDescriptionLength {
				slog.Warn("skill description truncated for the catalog",
					"name", info.Name, "source", source,
					"bytes", len(info.Description), "max", MaxDescriptionLength)
				info.Description = clampToBytes(info.Description, MaxDescriptionLength)
			}
			if len(info.Command) > MaxCommandLength {
				slog.Warn("skill command too long, dropping it from the catalog entry",
					"name", info.Name, "source", source, "bytes", len(info.Command))
				info.Command = ""
			}
			if err := info.validate(); err != nil {
				slog.Warn("invalid skill from "+source, "name", info.Name, "error", err)
				continue
			}
			if seen[info.Name] {
				continue
			}
			seen[info.Name] = true
			skills = append(skills, info)
		}
	}

	// Priority: workspace > global > builtin
	addSkills(sl.workspaceSkills, "workspace")
	addSkills(sl.globalSkills, "global")
	addSkills(sl.builtinSkills, "builtin")

	return skills
}

func (sl *SkillsLoader) LoadSkill(name string) (string, bool) {
	if err := ValidateSkillName(name); err != nil {
		return "", false
	}

	// 1. load from workspace skills first (project-level)
	if sl.workspaceSkills != "" {
		skillFile := filepath.Join(sl.workspaceSkills, name, "SKILL.md")
		if content, err := os.ReadFile(skillFile); err == nil {
			return sl.stripFrontmatter(string(content)), true
		}
	}

	// 2. then load from global skills (~/.picoclaw/skills)
	if sl.globalSkills != "" {
		skillFile := filepath.Join(sl.globalSkills, name, "SKILL.md")
		if content, err := os.ReadFile(skillFile); err == nil {
			return sl.stripFrontmatter(string(content)), true
		}
	}

	// 3. finally load from builtin skills
	if sl.builtinSkills != "" {
		skillFile := filepath.Join(sl.builtinSkills, name, "SKILL.md")
		if content, err := os.ReadFile(skillFile); err == nil {
			return sl.stripFrontmatter(string(content)), true
		}
	}

	return "", false
}

func (sl *SkillsLoader) LoadSkillsForContext(skillNames []string) string {
	if len(skillNames) == 0 {
		return ""
	}

	var parts []string
	for _, name := range skillNames {
		content, ok := sl.LoadSkill(name)
		if ok {
			parts = append(parts, fmt.Sprintf("### Skill: %s\n\n%s", name, content))
		}
	}

	return strings.Join(parts, "\n\n---\n\n")
}

func (sl *SkillsLoader) BuildSkillsSummary() string {
	allSkills := sl.ListSkills()
	if len(allSkills) == 0 {
		return ""
	}

	var lines []string
	lines = append(lines, "<skills>")
	for _, s := range allSkills {
		escapedName := escapeXML(s.Name)
		escapedDesc := escapeXML(s.Description)
		escapedPath := escapeXML(s.Path)

		lines = append(lines, fmt.Sprintf("  <skill>"))
		lines = append(lines, fmt.Sprintf("    <name>%s</name>", escapedName))
		lines = append(lines, fmt.Sprintf("    <description>%s</description>", escapedDesc))
		if s.Command != "" {
			lines = append(lines, fmt.Sprintf("    <command>%s</command>", escapeXML(s.Command)))
		}
		lines = append(lines, fmt.Sprintf("    <location>%s</location>", escapedPath))
		lines = append(lines, fmt.Sprintf("    <source>%s</source>", s.Source))
		if s.Confirm {
			lines = append(lines, "    <confirm>true</confirm>")
		}
		lines = append(lines, "  </skill>")
	}
	lines = append(lines, "</skills>")

	return strings.Join(lines, "\n")
}

func (sl *SkillsLoader) getSkillMetadata(skillPath string) *SkillMetadata {
	content, err := os.ReadFile(skillPath)
	if err != nil {
		logger.WarnCF("skills", "Failed to read skill metadata",
			map[string]any{
				"skill_path": skillPath,
				"error":      err.Error(),
			})
		return nil
	}

	frontmatter, bodyContent := splitFrontmatter(string(content))
	dirName := filepath.Base(filepath.Dir(skillPath))
	title, bodyDescription := extractMarkdownMetadata(bodyContent)

	metadata := &SkillMetadata{
		Name:        dirName,
		Description: bodyDescription,
	}
	if title != "" && namePattern.MatchString(title) && len(title) <= MaxNameLength {
		metadata.Name = title
	}

	if frontmatter == "" {
		return metadata
	}

	// Try JSON first (for backward compatibility)
	var jsonMeta struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Confirm     bool   `json:"confirm"`
		Command     string `json:"command"`
	}
	if err := json.Unmarshal([]byte(frontmatter), &jsonMeta); err == nil {
		if jsonMeta.Name != "" {
			metadata.Name = jsonMeta.Name
		}
		if jsonMeta.Description != "" {
			metadata.Description = jsonMeta.Description
		}
		metadata.Confirm = jsonMeta.Confirm
		metadata.Command = jsonMeta.Command
		return metadata
	}

	// Fall back to simple YAML parsing
	yamlMeta := sl.parseSimpleYAML(frontmatter, skillPath)
	if name := yamlMeta["name"]; name != "" {
		metadata.Name = name
	}
	if description := yamlMeta["description"]; description != "" {
		metadata.Description = description
	}
	if yamlMeta["confirm"] == "true" {
		metadata.Confirm = true
	}
	if command := yamlMeta["command"]; command != "" {
		metadata.Command = command
	}
	return metadata
}

func extractMarkdownMetadata(content string) (title, description string) {
	p := parser.NewWithExtensions(parser.CommonExtensions)
	doc := markdown.Parse([]byte(content), p)
	if doc == nil {
		return "", ""
	}

	ast.WalkFunc(doc, func(node ast.Node, entering bool) ast.WalkStatus {
		if !entering {
			return ast.GoToNext
		}

		switch n := node.(type) {
		case *ast.Heading:
			if title == "" && n.Level == 1 {
				title = nodeText(n)
				if title != "" && description != "" {
					return ast.Terminate
				}
			}
		case *ast.Paragraph:
			if description == "" {
				description = nodeText(n)
				if title != "" && description != "" {
					return ast.Terminate
				}
			}
		}
		return ast.GoToNext
	})

	return title, description
}

func nodeText(n ast.Node) string {
	var b strings.Builder
	ast.WalkFunc(n, func(node ast.Node, entering bool) ast.WalkStatus {
		if !entering {
			return ast.GoToNext
		}

		switch t := node.(type) {
		case *ast.Text:
			b.Write(t.Literal)
		case *ast.Code:
			b.Write(t.Literal)
		case *ast.Softbreak, *ast.Hardbreak, *ast.NonBlockingSpace:
			b.WriteByte(' ')
		}
		return ast.GoToNext
	})
	return strings.Join(strings.Fields(b.String()), " ")
}

// parseSimpleYAML parses YAML frontmatter and extracts known metadata fields.
// skillPath is used only for logging; pass "" when the source is not a file.
func (sl *SkillsLoader) parseSimpleYAML(content string, skillPath string) map[string]string {
	result := make(map[string]string)

	var meta struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
		Confirm     bool   `yaml:"confirm"`
		Command     string `yaml:"command"`
	}
	if err := yaml.Unmarshal([]byte(content), &meta); err != nil {
		// Loudly, because this used to be silent and cost four skills their entire
		// frontmatter: a plain YAML scalar may not contain ": ", so one colon in a
		// description throws away name, description and command, and the catalog
		// quietly falls back to the markdown body's first paragraph.
		slog.Warn("skill frontmatter is not valid YAML; every field in it was ignored "+
			"and the catalog fell back to the markdown body",
			"skill_path", skillPath, "error", err)
		return result
	}
	if meta.Name != "" {
		result["name"] = meta.Name
	}
	if meta.Description != "" {
		result["description"] = meta.Description
	}
	if meta.Confirm {
		result["confirm"] = "true"
	}
	if meta.Command != "" {
		result["command"] = meta.Command
	}

	return result
}

func (sl *SkillsLoader) extractFrontmatter(content string) string {
	frontmatter, _ := splitFrontmatter(content)
	return frontmatter
}

func (sl *SkillsLoader) stripFrontmatter(content string) string {
	_, body := splitFrontmatter(content)
	return body
}

func splitFrontmatter(content string) (frontmatter, body string) {
	normalized := string(parser.NormalizeNewlines([]byte(content)))
	lines := strings.Split(normalized, "\n")
	if len(lines) == 0 || lines[0] != "---" {
		return "", content
	}

	end := -1
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			end = i
			break
		}
	}
	if end == -1 {
		return "", content
	}

	frontmatter = strings.Join(lines[1:end], "\n")
	body = strings.Join(lines[end+1:], "\n")
	body = strings.TrimLeft(body, "\n")
	return frontmatter, body
}

func escapeXML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}
