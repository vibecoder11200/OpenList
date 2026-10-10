package mediafire

import (
	"strings"
	"testing"
)

func TestPollDecision(t *testing.T) {
	cases := []struct {
		name             string
		status, result   string
		fileError        string
		quickKey         string
		want             pollOutcome
		wantReasonSubstr string
	}{
		{name: "assembly pending", status: "18", want: pollContinue},
		{name: "assembling", status: "19", want: pollContinue},
		{name: "upload in progress", status: "3", want: pollContinue},
		{name: "verifying", status: "6", want: pollContinue},
		{name: "no record yet", status: "0", want: pollContinue},
		{
			name: "complete with quickkey", status: "99", quickKey: "abc123",
			want: pollSuccess,
		},
		{
			name: "complete but rejected storage limit", status: "99", quickKey: "k", fileError: "15",
			want:             pollTerminal,
			wantReasonSubstr: "storage limit reached",
		},
		{
			name: "complete but virus", status: "99", fileError: "5",
			want:             pollTerminal,
			wantReasonSubstr: "virus found",
		},
		{
			name: "complete without quickkey", status: "99",
			want:             pollTerminal,
			wantReasonSubstr: "no quickkey",
		},
		{
			name: "invalid upload key", status: "3", result: "-20",
			want:             pollTerminal,
			wantReasonSubstr: "upload key rejected",
		},
		{
			name: "upload key not found", status: "0", result: "-80",
			want:             pollTerminal,
			wantReasonSubstr: "-80",
		},
		{
			name: "unknown fileerror code surfaces raw", status: "99", fileError: "42",
			want:             pollTerminal,
			wantReasonSubstr: "code 42",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outcome, reason := pollDecision(tc.status, tc.result, tc.fileError, tc.quickKey)
			if outcome != tc.want {
				t.Fatalf("pollDecision(%q,%q,%q,%q) = %v, want %v (reason %q)",
					tc.status, tc.result, tc.fileError, tc.quickKey, outcome, tc.want, reason)
			}
			if tc.wantReasonSubstr != "" && !strings.Contains(reason, tc.wantReasonSubstr) {
				t.Fatalf("reason %q does not contain %q", reason, tc.wantReasonSubstr)
			}
		})
	}
}
