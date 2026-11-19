package lists

import (
	"slices"

	"github.com/devforth/exception-monitoring/internal/platform/config"
)

// Option is a checkbox of a multi-select: a project or a level.
type Option struct {
	Name     string
	Selected bool
}

// Options marks the chosen names among all of them.
func Options(names, chosen []string) []Option {
	result := make([]Option, 0, len(names))

	for _, name := range names {
		result = append(result, Option{Name: name, Selected: slices.Contains(chosen, name)})
	}

	return result
}

// Picker is a multi-select filter (the "picker" partial): a dropdown of
// checkboxes sent as ?<Param>=a&<Param>=b. The field says only how many are
// chosen ("All projects", "Projects: 2"); the values are in the chips.
type Picker struct {
	Param   string // the query parameter
	Kind    string // the badge kind of the options ("project", "level")
	All     string // "All projects": the field and the entry that clears the choice
	Label   string // "Projects" of "Projects: 2"
	Options []Option
	Count   int // how many are chosen
}

// Shown: a picker with one option and nothing chosen would filter nothing.
func (picker Picker) Shown() bool {
	return len(picker.Options) > 1 || picker.Count > 0
}

func NewPicker(param, kind, all, label string, names, chosen []string) Picker {
	picker := Picker{Param: param, Kind: kind, All: all, Label: label, Options: Options(names, chosen)}

	for _, option := range picker.Options {
		if option.Selected {
			picker.Count++
		}
	}

	return picker
}

// NewLevelPicker offers the levels of all (highest first) that some record
// has, and those the link chose: others would find nothing.
func NewLevelPicker(all, present, chosen []string) Picker {
	offered := slices.DeleteFunc(slices.Clone(all), func(level string) bool {
		return !slices.Contains(present, level) && !slices.Contains(chosen, level)
	})

	return NewPicker("level", "level", "All levels", "Levels", offered, chosen)
}

// LevelOptions lists the known levels highest first, chosen when recorded
// (nil records every level).
func LevelOptions(known, recorded []string) []Option {
	levels := config.HighestFirst(known)

	if recorded == nil {
		return Options(levels, levels)
	}

	return Options(levels, recorded)
}
