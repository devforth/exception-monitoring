package ui

import "slices"

// option is a checkbox of a multi-select: a project or a level.
type option struct {
	Name     string
	Selected bool
}

// options marks the chosen names among all of them.
func options(names, chosen []string) []option {
	result := make([]option, 0, len(names))

	for _, name := range names {
		result = append(result, option{Name: name, Selected: slices.Contains(chosen, name)})
	}

	return result
}

// pickerView is a multi-select filter (the "picker" partial): a dropdown of
// checkboxes sent as ?<Param>=a&<Param>=b. The field says only how many are
// chosen ("All projects", "Projects: 2"); the values are in the chips.
type pickerView struct {
	Param   string // the query parameter
	Kind    string // the badge kind of the options ("project", "level")
	All     string // "All projects": the field and the entry that clears the choice
	Label   string // "Projects" of "Projects: 2"
	Options []option
	Count   int // how many are chosen
}

func newPicker(param, kind, all, label string, names, chosen []string) pickerView {
	picker := pickerView{Param: param, Kind: kind, All: all, Label: label, Options: options(names, chosen)}

	for _, option := range picker.Options {
		if option.Selected {
			picker.Count++
		}
	}

	return picker
}

// highestFirst lists levels for display: config keeps them lowest first.
func highestFirst(levels []string) []string {
	reversed := slices.Clone(levels)
	slices.Reverse(reversed)

	return reversed
}
