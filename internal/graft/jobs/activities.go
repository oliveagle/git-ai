package jobs

// Activities are the activities of the jobs of a node.
//
// An activity does the work of a job: it reads the files of the node, it
// reaches the servers, and it writes what the job produced. It is the part
// of a job that is allowed to fail and to be retried, and it is the part
// that holds the credentials of the node.
type Activities struct {
	// Options is the configuration of the node the activities work on.
	Options Options
}
