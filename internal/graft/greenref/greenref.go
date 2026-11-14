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
