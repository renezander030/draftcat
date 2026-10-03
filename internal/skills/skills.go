// Package skills loads and holds the YAML prompt templates in the skills/
// directory. The engine resolves a step's `skill:` reference against the
// registry; the validator checks every referenced skill exists.
package skills

import (
	"log"
	"os"
	"path/filepath"

	"github.com/renezander030/draftcat/internal/outputschema"
	"gopkg.in/yaml.v3"
)

type SkillDef struct {
	Name         string              `yaml:"name"`
	Description  string              `yaml:"description"`
	Role         string              `yaml:"role"`
	Prompt       string              `yaml:"prompt"`
	OutputSchema outputschema.Schema `yaml:"output_schema"`
}

// SkillRegistry loads and holds all skills from the skills/ directory.
type SkillRegistry struct {
	skills map[string]*SkillDef
}

// LoadSkills reads every skills/*.yaml into a registry. A missing directory is
// not an error (returns an empty registry); unreadable/unparseable files are
// logged and skipped.
func LoadSkills(dir string) (*SkillRegistry, error) {
	reg := &SkillRegistry{skills: make(map[string]*SkillDef)}
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return reg, nil // no skills dir is fine
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			log.Printf("[skills] failed to read %s: %v", f, err)
			continue
		}
		var skill SkillDef
		if err := yaml.Unmarshal(data, &skill); err != nil {
			log.Printf("[skills] failed to parse %s: %v", f, err)
			continue
		}
		reg.skills[skill.Name] = &skill
		log.Printf("[skills] loaded: %s (%s)", skill.Name, skill.Description)
	}
	return reg, nil
}

func (r *SkillRegistry) Get(name string) (*SkillDef, bool) {
	s, ok := r.skills[name]
	return s, ok
}

func (r *SkillRegistry) List() []*SkillDef {
	var out []*SkillDef
	for _, s := range r.skills {
		out = append(out, s)
	}
	return out
}
