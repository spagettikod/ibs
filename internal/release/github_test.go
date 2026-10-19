package release

import "testing"

func TestGitHubNotes(t *testing.T) {
	body := `[{"tag_name":"v1.8.3","body":"### New\n\n- Restore with the phrase"},{"tag_name":"relay-v1.5.0","body":"x"},{"tag_name":"v1.8.2","body":""}]`
	n := GitHubNotes([]byte(body))
	if len(n) != 1 || n["1.8.3"] != "### New\n\n- Restore with the phrase" {
		t.Fatalf("GitHubNotes = %v", n)
	}
}
