// Copyright 2019 The Gitea Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

// Package private includes all internal routes. The package name internal is ideal but Golang is not allowed, so we use private as package name instead.
package private

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"code.gitea.io/gitea/models"
	gitea_context "code.gitea.io/gitea/modules/context"
	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/private"
	"code.gitea.io/gitea/modules/web"
	pull_service "code.gitea.io/gitea/services/pull"
)

type preReceiveContext struct {
	*gitea_context.PrivateContext
	user *models.User
	perm models.Permission

	canCreatePullRequest        bool
	checkedCanCreatePullRequest bool

	canWriteCode        bool
	checkedCanWriteCode bool

	protectedTags    []*models.ProtectedTag
	gotProtectedTags bool

	env []string

	opts *private.HookOptions
}

// User gets or loads User
func (ctx *preReceiveContext) User() *models.User {
	if ctx.user == nil {
		ctx.user, ctx.perm = loadUserAndPermission(ctx.PrivateContext, ctx.opts.UserID)
	}
	return ctx.user
}

// Perm gets or loads Perm
func (ctx *preReceiveContext) Perm() *models.Permission {
	if ctx.user == nil {
		ctx.user, ctx.perm = loadUserAndPermission(ctx.PrivateContext, ctx.opts.UserID)
	}
	return &ctx.perm
}

// CanWriteCode returns true if can write code
func (ctx *preReceiveContext) CanWriteCode() bool {
	if !ctx.checkedCanWriteCode {
		ctx.canWriteCode = ctx.Perm().CanWrite(models.UnitTypeCode)
		ctx.checkedCanWriteCode = true
	}
	return ctx.canWriteCode
}

// AssertCanWriteCode returns true if can write code
func (ctx *preReceiveContext) AssertCanWriteCode() bool {
	if !ctx.CanWriteCode() {
		if ctx.Written() {
			return false
		}
		ctx.JSON(http.StatusForbidden, map[string]interface{}{
			"err": "User permission denied.",
		})
		return false
	}
	return true
}

// CanCreatePullRequest returns true if can create pull requests
func (ctx *preReceiveContext) CanCreatePullRequest() bool {
	if !ctx.checkedCanCreatePullRequest {
		ctx.canCreatePullRequest = ctx.Perm().CanRead(models.UnitTypePullRequests)
		ctx.checkedCanCreatePullRequest = true
	}
	return ctx.canCreatePullRequest
}

// AssertCanCreatePullRequest returns true if can create pull requests
func (ctx *preReceiveContext) AssertCreatePullRequest() bool {
	if !ctx.CanCreatePullRequest() {
		if ctx.Written() {
			return false
		}
		ctx.JSON(http.StatusForbidden, map[string]interface{}{
			"err": "User permission denied.",
		})
		return false
	}
	return true
}

// HookPreReceive checks whether a individual commit is acceptable
func HookPreReceive(ctx *gitea_context.PrivateContext) {
	opts := web.GetForm(ctx).(*private.HookOptions)

	ourCtx := &preReceiveContext{
		PrivateContext: ctx,
		env:            generateGitEnv(opts), // Generate git environment for checking commits
		opts:           opts,
	}

	// Iterate across the provided old commit IDs
	for i := range opts.OldCommitIDs {
		oldCommitID := opts.OldCommitIDs[i]
		newCommitID := opts.NewCommitIDs[i]
		refFullName := opts.RefFullNames[i]

		switch {
		case strings.HasPrefix(refFullName, git.BranchPrefix):
			preReceiveBranch(ourCtx, oldCommitID, newCommitID, refFullName)
		case strings.HasPrefix(refFullName, git.TagPrefix):
			preReceiveTag(ourCtx, oldCommitID, newCommitID, refFullName)
		case git.SupportProcReceive && strings.HasPrefix(refFullName, git.PullRequestPrefix):
			preReceivePullRequest(ourCtx, oldCommitID, newCommitID, refFullName)
		default:
			ourCtx.AssertCanWriteCode()
		}
		if ctx.Written() {
			return
		}
	}

	ctx.PlainText(http.StatusOK, []byte("ok"))
}

func preReceiveBranch(ctx *preReceiveContext, oldCommitID, newCommitID, refFullName string) {
	if !ctx.AssertCanWriteCode() {
		return
	}

	repo := ctx.Repo.Repository
	gitRepo := ctx.Repo.GitRepo
	branchName := strings.TrimPrefix(refFullName, git.BranchPrefix)

	if branchName == repo.DefaultBranch && newCommitID == git.EmptySHA {
		log.Warn("Forbidden: Branch: %s is the default branch in %-v and cannot be deleted", branchName, repo)
		ctx.JSON(http.StatusForbidden, private.Response{
			Err: fmt.Sprintf("branch %s is the default branch and cannot be deleted", branchName),
		})
		return
	}

	protectBranch, err := models.GetProtectedBranchBy(repo.ID, branchName)
	if err != nil {
		log.Error("Unable to get protected branch: %s in %-v Error: %v", branchName, repo, err)
		ctx.JSON(http.StatusInternalServerError, private.Response{
			Err: err.Error(),
		})
		return
	}

	// Allow pushes to non-protected branches
	if protectBranch == nil || !protectBranch.IsProtected() {
		return
	}

	// This ref is a protected branch.
	//
	// First of all we need to enforce absolutely:
	//
	// 1. Detect and prevent deletion of the branch
	if newCommitID == git.EmptySHA {
		log.Warn("Forbidden: Branch: %s in %-v is protected from deletion", branchName, repo)
		ctx.JSON(http.StatusForbidden, private.Response{
			Err: fmt.Sprintf("branch %s is protected from deletion", branchName),
		})
		return
	}

	// 2. Disallow force pushes to protected branches
	if git.EmptySHA != oldCommitID {
		output, err := git.NewCommand("rev-list", "--max-count=1", oldCommitID, "^"+newCommitID).RunInDirWithEnv(repo.RepoPath(), ctx.env)
		if err != nil {
			log.Error("Unable to detect force push between: %s and %s in %-v Error: %v", oldCommitID, newCommitID, repo, err)
			ctx.JSON(http.StatusInternalServerError, private.Response{
				Err: fmt.Sprintf("Fail to detect force push: %v", err),
			})
			return
		} else if len(output) > 0 {
			log.Warn("Forbidden: Branch: %s in %-v is protected from force push", branchName, repo)
			ctx.JSON(http.StatusForbidden, private.Response{
				Err: fmt.Sprintf("branch %s is protected from force push", branchName),
			})
			return

		}
	}

	// 3. Enforce require signed commits
	if protectBranch.RequireSignedCommits {
		err := verifyCommits(oldCommitID, newCommitID, gitRepo, ctx.env)
		if err != nil {
			if !isErrUnverifiedCommit(err) {
				log.Error("Unable to check commits from %s to %s in %-v: %v", oldCommitID, newCommitID, repo, err)
				ctx.JSON(http.StatusInternalServerError, private.Response{
					Err: fmt.Sprintf("Unable to check commits from %s to %s: %v", oldCommitID, newCommitID, err),
				})
				return
			}
			unverifiedCommit := err.(*errUnverifiedCommit).sha
			log.Warn("Forbidden: Branch: %s in %-v is protected from unverified commit %s", branchName, repo, unverifiedCommit)
			ctx.JSON(http.StatusForbidden, private.Response{
				Err: fmt.Sprintf("branch %s is protected from unverified commit %s", branchName, unverifiedCommit),
			})
			return
		}
	}

	// Now there are several tests which can be overridden:
	//
	// 4. Check protected file patterns - this is overridable from the UI
	changedProtectedfiles := false
	protectedFilePath := ""

	globs := protectBranch.GetProtectedFilePatterns()
	if len(globs) > 0 {
		_, err := pull_service.CheckFileProtection(oldCommitID, newCommitID, globs, 1, ctx.env, gitRepo)
		if err != nil {
			if !models.IsErrFilePathProtected(err) {
				log.Error("Unable to check file protection for commits from %s to %s in %-v: %v", oldCommitID, newCommitID, repo, err)
				ctx.JSON(http.StatusInternalServerError, private.Response{
					Err: fmt.Sprintf("Unable to check file protection for commits from %s to %s: %v", oldCommitID, newCommitID, err),
				})
				return

			}

			changedProtectedfiles = true
			protectedFilePath = err.(models.ErrFilePathProtected).Path
		}
	}

	// 5. Check if the doer is allowed to push
	canPush := false
	if ctx.opts.IsDeployKey {
		canPush = !changedProtectedfiles && protectBranch.CanPush && (!protectBranch.EnableWhitelist || protectBranch.WhitelistDeployKeys)
	} else {
		canPush = !changedProtectedfiles && protectBranch.CanUserPush(ctx.opts.UserID)
	}

	// 6. If we're not allowed to push directly
	if !canPush {
		// Is this is a merge from the UI/API?
		if ctx.opts.PullRequestID == 0 {
			// 6a. If we're not merging from the UI/API then there are two ways we got here:
			//
			// We are changing a protected file and we're not allowed to do that
			if changedProtectedfiles {
				log.Warn("Forbidden: Branch: %s in %-v is protected from changing file %s", branchName, repo, protectedFilePath)
				ctx.JSON(http.StatusForbidden, private.Response{
					Err: fmt.Sprintf("branch %s is protected from changing file %s", branchName, protectedFilePath),
				})
				return
			}

			// Allow commits that only touch unprotected files
			globs := protectBranch.GetUnprotectedFilePatterns()
			if len(globs) > 0 {
				unprotectedFilesOnly, err := pull_service.CheckUnprotectedFiles(oldCommitID, newCommitID, globs, ctx.env, gitRepo)
				if err != nil {
					log.Error("Unable to check file protection for commits from %s to %s in %-v: %v", oldCommitID, newCommitID, repo, err)
					ctx.JSON(http.StatusInternalServerError, private.Response{
						Err: fmt.Sprintf("Unable to check file protection for commits from %s to %s: %v", oldCommitID, newCommitID, err),
					})
					return
				}
				if unprotectedFilesOnly {
					// Commit only touches unprotected files, this is allowed
					return
				}
			}

			// Or we're simply not able to push to this protected branch
			log.Warn("Forbidden: User %d is not allowed to push to protected branch: %s in %-v", ctx.opts.UserID, branchName, repo)
			ctx.JSON(http.StatusForbidden, private.Response{
				Err: fmt.Sprintf("Not allowed to push to protected branch %s", branchName),
			})
			return
		}
		// 6b. Merge (from UI or API)

		// Get the PR, user and permissions for the user in the repository
		pr, err := models.GetPullRequestByID(ctx.opts.PullRequestID)
		if err != nil {
			log.Error("Unable to get PullRequest %d Error: %v", ctx.opts.PullRequestID, err)
			ctx.JSON(http.StatusInternalServerError, private.Response{
				Err: fmt.Sprintf("Unable to get PullRequest %d Error: %v", ctx.opts.PullRequestID, err),
			})
			return
		}

		// Now check if the user is allowed to merge PRs for this repository
		// Note: we can use ctx.perm and ctx.user directly as they will have been loaded above
		allowedMerge, err := pull_service.IsUserAllowedToMerge(pr, ctx.perm, ctx.user)
		if err != nil {
			log.Error("Error calculating if allowed to merge: %v", err)
			ctx.JSON(http.StatusInternalServerError, private.Response{
				Err: fmt.Sprintf("Error calculating if allowed to merge: %v", err),
			})
			return
		}

		if !allowedMerge {
			log.Warn("Forbidden: User %d is not allowed to push to protected branch: %s in %-v and is not allowed to merge pr #%d", ctx.opts.UserID, branchName, repo, pr.Index)
			ctx.JSON(http.StatusForbidden, private.Response{
				Err: fmt.Sprintf("Not allowed to push to protected branch %s", branchName),
			})
			return
		}

		// If we're an admin for the repository we can ignore status checks, reviews and override protected files
		if ctx.perm.IsAdmin() {
			return
		}

		// Now if we're not an admin - we can't overwrite protected files so fail now
		if changedProtectedfiles {
			log.Warn("Forbidden: Branch: %s in %-v is protected from changing file %s", branchName, repo, protectedFilePath)
			ctx.JSON(http.StatusForbidden, private.Response{
				Err: fmt.Sprintf("branch %s is protected from changing file %s", branchName, protectedFilePath),
			})
			return
		}

		// Check all status checks and reviews are ok
		if err := pull_service.CheckPRReadyToMerge(pr, true); err != nil {
			if models.IsErrNotAllowedToMerge(err) {
				log.Warn("Forbidden: User %d is not allowed push to protected branch %s in %-v and pr #%d is not ready to be merged: %s", ctx.opts.UserID, branchName, repo, pr.Index, err.Error())
				ctx.JSON(http.StatusForbidden, private.Response{
					Err: fmt.Sprintf("Not allowed to push to protected branch %s and pr #%d is not ready to be merged: %s", branchName, ctx.opts.PullRequestID, err.Error()),
				})
				return
			}
			log.Error("Unable to check if mergable: protected branch %s in %-v and pr #%d. Error: %v", ctx.opts.UserID, branchName, repo, pr.Index, err)
			ctx.JSON(http.StatusInternalServerError, private.Response{
				Err: fmt.Sprintf("Unable to get status of pull request %d. Error: %v", ctx.opts.PullRequestID, err),
			})
			return
		}
	}
}

func preReceiveTag(ctx *preReceiveContext, oldCommitID, newCommitID, refFullName string) {
	if !ctx.AssertCanWriteCode() {
		return
	}

	tagName := strings.TrimPrefix(refFullName, git.TagPrefix)

	if !ctx.gotProtectedTags {
		var err error
		ctx.protectedTags, err = ctx.Repo.Repository.GetProtectedTags()
		if err != nil {
			log.Error("Unable to get protected tags for %-v Error: %v", ctx.Repo.Repository, err)
			ctx.JSON(http.StatusInternalServerError, private.Response{
				Err: err.Error(),
			})
			return
		}
		ctx.gotProtectedTags = true
	}

	isAllowed, err := models.IsUserAllowedToControlTag(ctx.protectedTags, tagName, ctx.opts.UserID)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, private.Response{
			Err: err.Error(),
		})
		return
	}
	if !isAllowed {
		log.Warn("Forbidden: Tag %s in %-v is protected", tagName, ctx.Repo.Repository)
		ctx.JSON(http.StatusForbidden, private.Response{
			Err: fmt.Sprintf("Tag %s is protected", tagName),
		})
		return
	}
}

func preReceivePullRequest(ctx *preReceiveContext, oldCommitID, newCommitID, refFullName string) {
	if !ctx.AssertCreatePullRequest() {
		return
	}

	if ctx.Repo.Repository.IsEmpty {
		ctx.JSON(http.StatusForbidden, map[string]interface{}{
			"err": "Can't create pull request for an empty repository.",
		})
		return
	}

	if ctx.opts.IsWiki {
		ctx.JSON(http.StatusForbidden, map[string]interface{}{
			"err": "Pull requests are not suppported on the wiki.",
		})
		return
	}

	baseBranchName := refFullName[len(git.PullRequestPrefix):]

	baseBranchExist := false
	if ctx.Repo.GitRepo.IsBranchExist(baseBranchName) {
		baseBranchExist = true
	}

	if !baseBranchExist {
		for p, v := range baseBranchName {
			if v == '/' && ctx.Repo.GitRepo.IsBranchExist(baseBranchName[:p]) && p != len(baseBranchName)-1 {
				baseBranchExist = true
				break
			}
		}
	}

	if !baseBranchExist {
		ctx.JSON(http.StatusForbidden, private.Response{
			Err: fmt.Sprintf("Unexpected ref: %s", refFullName),
		})
		return
	}
}

func generateGitEnv(opts *private.HookOptions) (env []string) {
	env = os.Environ()
	if opts.GitAlternativeObjectDirectories != "" {
		env = append(env,
			private.GitAlternativeObjectDirectories+"="+opts.GitAlternativeObjectDirectories)
	}
	if opts.GitObjectDirectory != "" {
		env = append(env,
			private.GitObjectDirectory+"="+opts.GitObjectDirectory)
	}
	if opts.GitQuarantinePath != "" {
		env = append(env,
			private.GitQuarantinePath+"="+opts.GitQuarantinePath)
	}
	return env
}

func loadUserAndPermission(ctx *gitea_context.PrivateContext, id int64) (user *models.User, perm models.Permission) {
	user, err := models.GetUserByID(id)
	if err != nil {
		log.Error("Unable to get User id %d Error: %v", id, err)
		ctx.JSON(http.StatusInternalServerError, private.Response{
			Err: fmt.Sprintf("Unable to get User id %d Error: %v", id, err),
		})
		return
	}

	perm, err = models.GetUserRepoPermission(ctx.Repo.Repository, user)
	if err != nil {
		log.Error("Unable to get Repo permission of repo %s/%s of User %s", ctx.Repo.Repository.OwnerName, ctx.Repo.Repository.Name, user.Name, err)
		ctx.JSON(http.StatusInternalServerError, private.Response{
			Err: fmt.Sprintf("Unable to get Repo permission of repo %s/%s of User %s: %v", ctx.Repo.Repository.OwnerName, ctx.Repo.Repository.Name, user.Name, err),
		})
		return
	}

	return
}
