package cli

import (
	"reflect"
	"testing"
)

func TestParseFeedbackArgs(t *testing.T) {
	cases := []struct {
		args []string
		want FeedbackArgs
	}{
		{[]string{"deploy", "failed", "--command", "deploy", "--from=user"}, FeedbackArgs{Message: "deploy failed", Command: "deploy", From: "user"}},
		{[]string{"--command=db migrate", "x", "--", "--from", "agent"}, FeedbackArgs{Message: "x --from agent", Command: "db migrate"}},
		{[]string{"-h"}, FeedbackArgs{Help: true}},
	}
	for _, c := range cases {
		if got := ParseFeedbackArgs(c.args); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%v: %+v, want %+v", c.args, got, c.want)
		}
	}
}
