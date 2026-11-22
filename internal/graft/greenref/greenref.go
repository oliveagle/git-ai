// Package greenref names the references that hold the verification
// checkpoints of the branches: the checkpoint itself, and the tree it
// certifies.
//
// The checkpoint of a branch is the commit whose tree passed the verification
// of the branch (refs/graft/green/<branch>). The tree it certifies
// (refs/graft/green-tree/<branch>) is the tree the suite ran on: it is the
// tree of the checkpoint commit when the whole tree was tested, and it is
// absent when the publisher certified no tree at all.
//
// The two are read together, and the checkpoint only certifies the whole tree
// when the tree it names is the tree of the commit it names. A publisher that
// moves the checkpoint without moving the tree leaves a checkpoint that
// certifies nothing, which the branch policy refuses when the rule of the
// branch asks for the whole tree.
package greenref

import "strings"

// Prefix is the namespace of the verification checkpoints of the branches.
const Prefix = "refs/graft/green/"

// TreePrefix is the namespace of the trees the checkpoints certify.
const TreePrefix = "refs/graft/green-tree/"

// SuitesPrefix is the namespace of the suites a checkpoint certifies: one
// reference per suite and branch, each naming the commit the suite ran on.
const SuitesPrefix = "refs/graft/green-suite/"

// Name returns the name of the checkpoint reference of the branch.
func Name(branch string) string {
	return Prefix + strings.TrimPrefix(branch, "refs/heads/")
}

// TreeName returns the name of the tree reference of the branch.
func TreeName(branch string) string {
	return TreePrefix + strings.TrimPrefix(branch, "refs/heads/")
}

// TreeNameFor returns the name of the tree reference that goes with the
// checkpoint reference name: the tree of the same branch. A publisher that
// moves a checkpoint under another name moves the tree of that name with it.
func TreeNameFor(checkpointRef string) string {
	return strings.Replace(checkpointRef, Prefix, TreePrefix, 1)
}

// ValidSuiteName reports whether name is usable as a suite name: a reference
// path component, without a slash, that names a suite of a project. The suite
// is the first component of the reference that holds it, so a name with a
// slash in it would name a branch instead.
func ValidSuiteName(name string) bool {
	if name == "" || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".lock") {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\/", r) {
			return false
		}
	}
	return true
}

// SuitesName returns the name of the reference that holds the commit the
// suite certified on the branch.
//
// The suite is the first component of the name and the branch is the rest, so
// a branch whose name holds a slash does not make two suites one: the suite
// "unit" on the branch "feature/x" is refs/graft/green-suite/unit/feature/x,
// and the suite "unit" on the branch "feature" is
// refs/graft/green-suite/unit/feature.
func SuitesName(suite, branch string) string {
	return SuitesPrefix + suite + "/" + strings.TrimPrefix(branch, "refs/heads/")
}
