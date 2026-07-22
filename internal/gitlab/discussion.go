package gitlab

// Discussion is the subset of a GitLab MR discussion the command path needs:
// the identity and body of its root (first) note. (Populated in a later task.)
type Discussion struct {
	RootNoteAuthorID int64
	RootNoteBody     string
}
