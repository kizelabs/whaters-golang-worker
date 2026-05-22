package lease

import "testing"

func TestSanitizeIdentifier(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "empty defaults to public", input: "", want: "public"},
		{name: "simple", input: "public", want: "public"},
		{name: "underscore", input: "wa_schema", want: "wa_schema"},
		{name: "starts with number invalid", input: "1schema", wantErr: true},
		{name: "dash invalid", input: "wa-schema", wantErr: true},
		{name: "space invalid", input: "wa schema", wantErr: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := sanitizeIdentifier(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for input %q", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("expected %q, got %q", tt.want, got)
			}
		})
	}
}
