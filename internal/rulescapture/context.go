package rulescapture

import (
	"pi-gateway/internal/rules"
	"sort"
)

// markMissingContext distinguishes absent historical facts from JSON false/null.
// Engine-provided model/event_type remain available when their explicit fields exist.
func markMissingContext(input *rules.Input, response bool) {
	if input == nil {
		return
	}
	present := func(path string) bool { _, ok := input.Context[path]; return ok }
	missing := map[string]bool{}
	for _, path := range []string{"request_path", "request_method", "client_protocol", "api_key_id"} {
		if !present(path) {
			missing["/context/"+path] = true
		}
	}
	if response {
		markAbsentContext(input.Context, missing, []string{"account_id", "upstream_protocol"})
	}
	if input.Model == "" {
		missing["/context/model"] = true
	}
	if !present("original_model") && (response || input.Model == "") {
		missing["/context/original_model"] = true
	}
	if input.ClientBody == nil {
		missing["/client"] = true
	}
	for _, path := range input.Unavailable {
		missing[path] = true
	}
	input.Unavailable = input.Unavailable[:0]
	for path := range missing {
		input.Unavailable = append(input.Unavailable, path)
	}
	sort.Strings(input.Unavailable)
}

func markAbsentContext(context map[string]any, missing map[string]bool, paths []string) {
	for _, path := range paths {
		if _, present := context[path]; !present {
			missing["/context/"+path] = true
		}
	}
}
