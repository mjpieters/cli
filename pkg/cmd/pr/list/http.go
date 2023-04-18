package list

import (
	"fmt"
	"net/http"

	"github.com/cli/cli/v2/api"
	"github.com/cli/cli/v2/internal/ghrepo"
	prShared "github.com/cli/cli/v2/pkg/cmd/pr/shared"
	"github.com/cli/cli/v2/pkg/set"
)

func shouldUseSearch(filters prShared.FilterOptions) bool {
	return filters.Draft != nil || filters.Author != "" || filters.Assignee != "" || filters.Search != "" || len(filters.Labels) > 0
}

type responsePage struct {
	Nodes    []api.PullRequest
	PageInfo struct {
		HasNextPage bool
		EndCursor   string
	}
	TotalCount int
}
type requester interface {
	Request(limit int, endCursor *string) (*responsePage, error)
}

// Fetch pull requests, handling GraphQL pagination and limits
func fetchPullRequests(r requester, limit int, autoMergeStatus *bool) (*api.PullRequestAndTotalCount, error) {
	pageLimit := 100
	if autoMergeStatus == nil {
		// Only fetch what is needed, but only if we are not filtering locally
		pageLimit = min(100, limit)
	}
	var endCursor *string = nil
	res := api.PullRequestAndTotalCount{TotalCount: -1}
	var check = make(map[int]struct{})
	removed := 0

loop:
	for {
		prData, err := r.Request(pageLimit, endCursor)
		if err != nil {
			return nil, err
		}
		res.TotalCount = prData.TotalCount

		for _, pr := range prData.Nodes {
			if _, exists := check[pr.Number]; exists && pr.Number > 0 {
				continue
			}
			check[pr.Number] = struct{}{}

			if autoMergeStatus != nil && (*autoMergeStatus == (pr.AutoMergeRequest == nil)) {
				// If there are multiple pages but a limit that's lower than the
				// total, then the total count is at best an upper bound. We
				// can't know how many PRs would be filtered on unloaded pages.
				removed += 1
				res.TotalCountIsUpperBound = true
				continue
			}

			if len(res.PullRequests) < limit {
				res.PullRequests = append(res.PullRequests, pr)
			}
			if len(res.PullRequests) == limit && autoMergeStatus == nil {
				break loop
			}
		}

		if !prData.PageInfo.HasNextPage {
			// If we paged through all results, we know that the total count is the actual
			// count after local filtering.
			res.TotalCountIsUpperBound = false
			break
		} else if len(res.PullRequests) == limit {
			break
		} else {
			endCursor = &prData.PageInfo.EndCursor
			if autoMergeStatus == nil {
				// fetch fewer if close to the limit, but only if we are not filtering locally
				pageLimit = min(pageLimit, limit-len(res.PullRequests))
			}
		}
	}

	res.TotalCount -= removed
	return &res, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type listResponse struct {
	Repository struct {
		PullRequests responsePage
	}
}

type listRequester struct {
	client    *api.Client
	hostname  string
	variables map[string]interface{}
	query     string
}

func (r *listRequester) Request(limit int, endCursor *string) (*responsePage, error) {
	r.variables["limit"] = limit
	if endCursor != nil {
		r.variables["endCursor"] = &endCursor
	}
	var data listResponse
	err := r.client.GraphQL(r.hostname, r.query, r.variables, &data)
	if err != nil {
		return nil, err
	}
	return &data.Repository.PullRequests, nil
}

func listPullRequests(httpClient *http.Client, repo ghrepo.Interface, filters prShared.FilterOptions, limit int) (*api.PullRequestAndTotalCount, error) {
	fields := set.NewStringSet()
	fields.AddValues(filters.Fields)
	if filters.AutoMergeStatus != nil {
		fields.Add("autoMergeRequest")
		filters.Fields = fields.ToSlice()
	}

	if shouldUseSearch(filters) {
		return searchPullRequests(httpClient, repo, filters, limit)
	}

	fragment := fmt.Sprintf("fragment pr on PullRequest{%s}", api.PullRequestGraphQL(filters.Fields))
	query := fragment + `
		query PullRequestList(
			$owner: String!,
			$repo: String!,
			$limit: Int!,
			$endCursor: String,
			$baseBranch: String,
			$headBranch: String,
			$state: [PullRequestState!] = OPEN
		) {
			repository(owner: $owner, name: $repo) {
				pullRequests(
					states: $state,
					baseRefName: $baseBranch,
					headRefName: $headBranch,
					first: $limit,
					after: $endCursor,
					orderBy: {field: CREATED_AT, direction: DESC}
				) {
					totalCount
					nodes {
						...pr
					}
					pageInfo {
						hasNextPage
						endCursor
					}
				}
			}
		}`

	variables := map[string]interface{}{
		"owner": repo.RepoOwner(),
		"repo":  repo.RepoName(),
	}

	switch filters.State {
	case "open":
		variables["state"] = []string{"OPEN"}
	case "closed":
		variables["state"] = []string{"CLOSED", "MERGED"}
	case "merged":
		variables["state"] = []string{"MERGED"}
	case "all":
		variables["state"] = []string{"OPEN", "CLOSED", "MERGED"}
	default:
		return nil, fmt.Errorf("invalid state: %s", filters.State)
	}

	if filters.BaseBranch != "" {
		variables["baseBranch"] = filters.BaseBranch
	}
	if filters.HeadBranch != "" {
		variables["headBranch"] = filters.HeadBranch
	}

	r := &listRequester{
		client:    api.NewClientFromHTTP(httpClient),
		hostname:  repo.RepoHost(),
		variables: variables,
		query:     query,
	}
	return fetchPullRequests(r, limit, filters.AutoMergeStatus)
}

type searchResponse struct {
	Search struct {
		Nodes    []api.PullRequest
		PageInfo struct {
			HasNextPage bool
			EndCursor   string
		}
		IssueCount int
	}
}

type searchRequester struct {
	client    *api.Client
	hostname  string
	variables map[string]interface{}
	query     string
}

func (r *searchRequester) Request(limit int, endCursor *string) (*responsePage, error) {
	r.variables["limit"] = limit
	if endCursor != nil {
		r.variables["endCursor"] = &endCursor
	}
	var data searchResponse
	err := r.client.GraphQL(r.hostname, r.query, r.variables, &data)
	if err != nil {
		return nil, err
	}
	pullRequests := &responsePage{
		Nodes:      data.Search.Nodes,
		PageInfo:   data.Search.PageInfo,
		TotalCount: data.Search.IssueCount,
	}
	return pullRequests, nil
}

func searchPullRequests(httpClient *http.Client, repo ghrepo.Interface, filters prShared.FilterOptions, limit int) (*api.PullRequestAndTotalCount, error) {
	fragment := fmt.Sprintf("fragment pr on PullRequest{%s}", api.PullRequestGraphQL(filters.Fields))
	query := fragment + `
		query PullRequestSearch(
			$q: String!,
			$limit: Int!,
			$endCursor: String,
		) {
			search(query: $q, type: ISSUE, first: $limit, after: $endCursor) {
				issueCount
				nodes {
					...pr
				}
				pageInfo {
					hasNextPage
					endCursor
				}
			}
		}`

	filters.Repo = ghrepo.FullName(repo)
	filters.Entity = "pr"
	q := prShared.SearchQueryBuild(filters)
	variables := map[string]interface{}{"q": q}

	r := &searchRequester{
		client:    api.NewClientFromHTTP(httpClient),
		hostname:  repo.RepoHost(),
		variables: variables,
		query:     query,
	}
	res, err := fetchPullRequests(r, limit, filters.AutoMergeStatus)
	res.SearchCapped = limit > 1000
	return res, err
}
