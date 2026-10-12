package backend

import (
	"strings"
	"testing"
)

func TestPropsNameTheirCapabilities(t *testing.T) {
	rows := []struct {
		props Props
		want  string
	}{
		{Props{}, ""},
		{Props{SupportsTools: true}, "tools"},
		{Props{Thinking: true, Vision: true}, "thinking,vision"},
		{Props{SupportsTools: true, Thinking: true, Vision: true, ContextLength: 4096}, "tools,thinking,vision"},
	}
	for _, row := range rows {
		got := row.props.Capabilities()
		if got == nil || strings.Join(got, ",") != row.want {
			t.Errorf("%+v: %q, want %q as a list that is never nil", row.props, got, row.want)
		}
	}
}
