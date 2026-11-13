package main

import (
	"github.com/feder-cr/git-ai/internal/graft/workspace"
)

// resolveTarget returns the server and the repository of a command that works
// on one repository, and the root of the workspace (empty when there is none).
//
// They come from the workspace when there is one: the repository is the one of
// the workspace, and repoPath must name it or be left out. Without a
// workspace they come from -address, -storage and -repo.
//
// command names the command in the error messages.
func resolveTarget(o *options, repoPath, command string) (target, string, error) {
	root, rootErr := workspaceRoot(o)
	if rootErr == nil {
		meta, metaErr := workspace.ReadMeta(root)
		if metaErr == nil {
			if repoPath != "" && repoPath != meta.Repo {
				return target{}, "", usagef("%s -repo %q is not the repository of the workspace (%q): leave -repo out inside a workspace",
					command, repoPath, meta.Repo)
			}
			return target{
				Address: pick(o.address, meta.Address),
				Token:   o.token,
				Storage: pick(o.storage, meta.Storage),
				Repo:    meta.Repo,
				User:    o.user,
			}, root, nil
		}
		rootErr = metaErr
	}
	if o.address == "" || repoPath == "" {
		return target{}, "", rootErr
	}
	return target{Address: o.address, Token: o.token, Storage: o.storage, Repo: repoPath, User: o.user}, "", nil
}
