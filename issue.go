package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/go-github/v74/github"
	"github.com/xanzy/go-gitlab"
)

func (p *project) migrateIssues(ctx context.Context) {
	var issues []*gitlab.Issue

	opts := &gitlab.ListProjectIssuesOptions{
		OrderBy: pointer("created_at"),
		Sort:    pointer("asc"),
	}

	logger.Debug("retrieving GitLab issues", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID)
	for {
		result, resp, err := gl.Issues.ListProjectIssues(p.project.ID, opts)
		if err != nil {
			sendErr(fmt.Errorf("retrieving gitlab issues: %v", err))
			return
		}

		issues = append(issues, result...)

		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}

	var successCount, failureCount int
	totalCount := len(issues)
	logger.Info("migrating issues from GitLab to GitHub", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "count", totalCount)
	for _, issue := range issues {
		if issue == nil {
			continue
		}

		if ok, err := p.migrateIssue(ctx, issue); err != nil {
			sendErr(err)
			failureCount++
		} else if ok {
			successCount++
		}
	}

	skippedCount := totalCount - successCount - failureCount

	logger.Info("migrated issues from GitLab to GitHub", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "successful", successCount, "failed", failureCount, "skipped", skippedCount)
}

func (p *project) migrateIssue(ctx context.Context, issue *gitlab.Issue) (bool, error) {
	// Check for context cancellation
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("preparing to migrate issue: %v", err)
	}

	// Find any existing GitHub issue already migrated from this GitLab issue.
	// GitHub returns pull requests from the issues endpoint too, so skip those.
	logger.Debug("searching for existing GitHub issue", "owner", p.githubPath[0], "repo", p.githubPath[1], "gitlab_issue_id", issue.IID)
	existingIssues, _, err := gh.Issues.ListByRepo(ctx, p.githubPath[0], p.githubPath[1], &github.IssueListByRepoOptions{State: "all"})
	if err != nil {
		return false, fmt.Errorf("listing github issues: %v", err)
	}

	var githubIssue *github.Issue
	for _, gi := range existingIssues {
		if gi == nil || gi.IsPullRequest() {
			continue
		}
		if strings.Contains(gi.GetBody(), fmt.Sprintf("**GitLab Issue Number** | %d", issue.IID)) {
			githubIssue = gi
			break
		}
	}

	githubAuthorName := ""
	if issue.Author != nil {
		githubAuthorName = issue.Author.Name

		author, err := getGitlabUser(issue.Author.Username)
		if err != nil {
			return false, fmt.Errorf("retrieving gitlab user: %v", err)
		}
		if author.WebsiteURL != "" {
			githubAuthorName = "@" + strings.TrimPrefix(strings.ToLower(author.WebsiteURL), "https://github.com/")
		}
	}

	description := issue.Description
	if strings.TrimSpace(description) == "" {
		description = "_No description_"
	}

	originalState := ""
	if !strings.EqualFold(issue.State, "opened") {
		originalState = fmt.Sprintf("> This issue was originally **%s** on GitLab", issue.State)
	}

	closeDate := ""
	if strings.EqualFold(issue.State, "closed") && issue.ClosedAt != nil {
		closeDate = fmt.Sprintf("\n> | **Date Originally Closed** | %s |", issue.ClosedAt.Format(dateFormat))
	}

	createdAt := ""
	if issue.CreatedAt != nil {
		createdAt = issue.CreatedAt.Format(dateFormat)
	}

	body := fmt.Sprintf(`> [!NOTE]
> This issue was migrated from GitLab
>
> |      |      |
> | ---- | ---- |
> | **Original Author** | %[1]s |
> | **GitLab Project** | [%[2]s/%[3]s](https://%[7]s/%[2]s/%[3]s) |
> | **GitLab Issue** | [#%[4]d](https://%[7]s/%[2]s/%[3]s/-/issues/%[4]d) |
> | **GitLab Issue Number** | %[4]d |
> | **Date Originally Opened** | %[5]s |%[6]s
> |      |      |
>
%[8]s

## Original Description

%[9]s`, githubAuthorName, p.gitlabPath[0], p.gitlabPath[1], issue.IID, createdAt, closeDate, gitlabDomain, originalState, description)

	var labels *[]string
	if len(issue.Labels) > 0 {
		l := []string(issue.Labels)
		labels = &l
	}

	newState := "open"
	if strings.EqualFold(issue.State, "closed") {
		newState = "closed"
	}

	if githubIssue == nil {
		logger.Info("creating issue", "owner", p.githubPath[0], "repo", p.githubPath[1], "gitlab_issue_id", issue.IID)
		issueRequest := &github.IssueRequest{
			Title:  &issue.Title,
			Body:   &body,
			Labels: labels,
		}
		if githubIssue, _, err = gh.Issues.Create(ctx, p.githubPath[0], p.githubPath[1], issueRequest); err != nil {
			return false, fmt.Errorf("creating issue: %v", err)
		}

		// GitHub always creates issues in the open state, so close afterwards if needed
		if newState == "closed" {
			closeRequest := &github.IssueRequest{State: pointer("closed")}
			if githubIssue, _, err = gh.Issues.Edit(ctx, p.githubPath[0], p.githubPath[1], githubIssue.GetNumber(), closeRequest); err != nil {
				return false, fmt.Errorf("closing issue: %v", err)
			}
		}
	} else {
		if githubIssue.GetTitle() != issue.Title || githubIssue.GetBody() != body || githubIssue.GetState() != newState {
			logger.Info("updating issue", "owner", p.githubPath[0], "repo", p.githubPath[1], "issue_number", githubIssue.GetNumber())
			issueRequest := &github.IssueRequest{
				Title:  &issue.Title,
				Body:   &body,
				State:  &newState,
				Labels: labels,
			}
			if githubIssue, _, err = gh.Issues.Edit(ctx, p.githubPath[0], p.githubPath[1], githubIssue.GetNumber(), issueRequest); err != nil {
				return false, fmt.Errorf("updating issue: %v", err)
			}
		} else {
			logger.Trace("existing issue is up-to-date", "owner", p.githubPath[0], "repo", p.githubPath[1], "issue_number", githubIssue.GetNumber())
		}
	}

	if err = p.migrateIssueComments(ctx, issue, githubIssue.GetNumber()); err != nil {
		return false, err
	}

	return true, nil
}

func (p *project) migrateIssueComments(ctx context.Context, issue *gitlab.Issue, githubIssueNumber int) error {
	var comments []*gitlab.Note
	opts := &gitlab.ListIssueNotesOptions{
		OrderBy: pointer("created_at"),
		Sort:    pointer("asc"),
	}

	logger.Debug("retrieving GitLab issue comments", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "issue_id", issue.IID)
	for {
		result, resp, err := gl.Notes.ListIssueNotes(p.project.ID, issue.IID, opts)
		if err != nil {
			return fmt.Errorf("listing issue notes: %v", err)
		}

		comments = append(comments, result...)

		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}

	logger.Debug("retrieving GitHub issue comments", "owner", p.githubPath[0], "repo", p.githubPath[1], "issue_number", githubIssueNumber)
	githubComments, _, err := gh.Issues.ListComments(ctx, p.githubPath[0], p.githubPath[1], githubIssueNumber, &github.IssueListCommentsOptions{Sort: pointer("created"), Direction: pointer("asc")})
	if err != nil {
		sendErr(fmt.Errorf("listing issue comments: %v", err))
		return nil
	}

	logger.Info("migrating issue comments from GitLab to GitHub", "owner", p.githubPath[0], "repo", p.githubPath[1], "issue_number", githubIssueNumber, "count", len(comments))

	for _, comment := range comments {
		if comment == nil || comment.System {
			continue
		}

		githubCommentAuthorName := comment.Author.Name

		commentAuthor, err := getGitlabUser(comment.Author.Username)
		if err != nil {
			return fmt.Errorf("retrieving gitlab user: %v", err)
		}
		if commentAuthor.WebsiteURL != "" {
			githubCommentAuthorName = "@" + strings.TrimPrefix(strings.ToLower(commentAuthor.WebsiteURL), "https://github.com/")
		}

		commentBody := fmt.Sprintf(`> [!NOTE]
> This comment was migrated from GitLab
>
> |      |      |
> | ---- | ---- |
> | **Original Author** | %[1]s |
> | **Note ID** | %[2]d |
> | **Date Originally Created** | %[3]s |
> |      |      |
>

## Original Comment

%[4]s`, githubCommentAuthorName, comment.ID, comment.CreatedAt.Format("Mon, 2 Jan 2006"), comment.Body)

		foundExistingComment := false
		for _, githubComment := range githubComments {
			if githubComment == nil {
				continue
			}

			if strings.Contains(githubComment.GetBody(), fmt.Sprintf("**Note ID** | %d", comment.ID)) {
				foundExistingComment = true

				if githubComment.Body == nil || *githubComment.Body != commentBody {
					logger.Debug("updating issue comment", "owner", p.githubPath[0], "repo", p.githubPath[1], "issue_number", githubIssueNumber, "comment_id", githubComment.GetID())
					githubComment.Body = &commentBody
					if _, _, err = gh.Issues.EditComment(ctx, p.githubPath[0], p.githubPath[1], githubComment.GetID(), githubComment); err != nil {
						return fmt.Errorf("updating issue comment: %v", err)
					}
				}
			}
		}

		if !foundExistingComment {
			logger.Debug("creating issue comment", "owner", p.githubPath[0], "repo", p.githubPath[1], "issue_number", githubIssueNumber)
			newComment := github.IssueComment{
				Body: &commentBody,
			}
			if _, _, err = gh.Issues.CreateComment(ctx, p.githubPath[0], p.githubPath[1], githubIssueNumber, &newComment); err != nil {
				return fmt.Errorf("creating issue comment: %v", err)
			}
		}
	}

	return nil
}
