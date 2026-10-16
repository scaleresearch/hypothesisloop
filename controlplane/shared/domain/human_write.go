package domain

// HumanWrite is a row of the retired human_writes table: the signed-in user behind one
// hypothesis, link or comment written before a record kept its actor. Subject is the hypothesis
// id, the comment id, or for a link "<from> <type> <to>".
type HumanWrite struct {
	UserID  string
	Kind    string
	Subject string
}
