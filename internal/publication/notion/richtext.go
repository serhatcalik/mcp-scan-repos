package notion

func normalColor(color string) string {
	if color == "default" {
		return ""
	}
	return color
}

func linkURL(item richText) string {
	if item.Text.Link == nil {
		return ""
	}
	return item.Text.Link.URL
}

// Ignore response-only defaults and text segmentation, not actual formatting edits.
func normalizedRichText(items []richText) []richText {
	var out []richText
	for _, item := range items {
		if item.Type == "" {
			item.Type = "text"
		}
		item.Annotations.Color = normalColor(item.Annotations.Color)
		if item.Text.Content == "" && item.Type == "text" {
			continue
		}
		if len(out) > 0 {
			previous := &out[len(out)-1]
			if previous.Type == item.Type && previous.Annotations == item.Annotations && linkURL(*previous) == linkURL(item) {
				previous.Text.Content += item.Text.Content
				continue
			}
		}
		out = append(out, item)
	}
	return out
}

func coalesceRichText(items []richText) []richText {
	var out []richText
	for _, item := range normalizedRichText(items) {
		out = append(out, richItems(item.Text.Content, item.Annotations, linkURL(item))...)
	}
	return out
}

func sameRichText(a, b []richText) bool {
	a, b = normalizedRichText(a), normalizedRichText(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Type != "text" || b[i].Type != "text" || a[i].Text.Content != b[i].Text.Content || a[i].Annotations != b[i].Annotations || linkURL(a[i]) != linkURL(b[i]) {
			return false
		}
	}
	return true
}
