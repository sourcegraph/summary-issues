package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

func main() {
	if err := testableMain(os.Stdout, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func testableMain(stdout io.Writer, args []string) error {
	opts, err := githubActionOptions(args)
	if err != nil {
		return err
	}

	e := opts.event
	fmt.Printf("event=%q\n", e.Name)
	switch e.Name {
	case "issues":
		if e.Issue.Labels.contains("summary") && isany(e.Action, "edited", "labeled", "unlabeled", "opened") {
			if err := updateSummaryIssue(opts, issue{
				id:     e.Issue.ID,
				title:  e.Issue.Title,
				labels: e.Issue.Labels,
			}); err != nil {
				return err
			}
		}

		switch e.Action {
		case "labeled", "unlabeled":
			if e.Label.Name == "summary" {
				return nil
			}
			return updateSummaryIssues(opts, labels{*e.Label})
		case "opened":
			labels := e.Issue.Labels.nonSummaryLabels()
			if len(labels) == 0 {
				return nil
			}
			return updateSummaryIssues(opts, labels)
		}
	case "issue_comment":
		labels := e.Issue.Labels.nonSummaryLabels()
		if len(labels) == 0 {
			return nil
		}
		// TODO: could fail fast using knowledge of comment and regex filter
		return updateSummaryIssues(opts, labels)
	case "schedule":
		// TODO: post comment?
		fallthrough
	default:
		fmt.Printf("nothing to update\n")
	}
	return nil
}

func githubActionOptions(args []string) (*options, error) {
	repo := os.Getenv("GITHUB_REPOSITORY")
	i := strings.IndexRune(repo, '/')
	if i < 1 {
		return nil, fmt.Errorf("invalid value for GITHUB_REPOSITORY env var: %q", repo)
	}

	user := repo[:i]

	path := os.Getenv("GITHUB_EVENT_PATH")
	if path == "" {
		return nil, fmt.Errorf("env var GITHUB_EVENT_PATH not set")
	}

	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("unable to read GitHub event json %s: %s", path, err)
	}

	name := os.Getenv("GITHUB_EVENT_NAME")
	if name == "" {
		return nil, fmt.Errorf("env var GITHUB_EVENT_NAME not set")
	}
	e := event{
		Name: name,
	}

	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("unable to decode GitHub event: %s\n%s", err, string(data))
	}

	opts := &options{
		user:  user,
		event: e,
	}

	flags := flag.NewFlagSet("summary", flag.ContinueOnError)
	re := flags.String("summaryCommentRegex", "", "The newest comment on an issue that matches this regular expression is used in the summary. If not provided, the most recent comment is always used.")

	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	opts.summaryCommentRegex, err = regexp.Compile(*re)

	if err != nil {
		return nil, err
	}
	return opts, nil
}

func updateSummaryIssues(opts *options, labels labels) error {
	issues, err := getSummaryIssues(opts.user, labels)
	if err != nil {
		return err
	}
	for _, i := range issues {
		if err := updateSummaryIssue(opts, issue{
			id:     i.ID,
			title:  i.Title,
			labels: i.Labels.Nodes,
		}); err != nil {
			return err
		}
	}
	return nil

}

func getSummaryIssues(user string, labels labels) ([]*graphqlIssue, error) {
	query := fmt.Sprintf("is:open user:%s label:summary %s", user, labels.queryFilter())
	fmt.Printf("searching summary issues %s\n", query)

	data := struct {
		Search struct {
			Nodes []*graphqlIssue `json:"nodes"`
		} `json:"search"`
	}{}
	err := graphql(`
		query SummaryIssues ($query: String!) {
			search(type: ISSUE, first: 100, query: $query) {
				nodes {
					... on Issue {
						id
						url
						title
						body
						author {
							login
						}
						labels(first: 100) {
							nodes {
								name
							}
						}
					}
				}
			}
		}
	`, map[string]interface{}{
		"query": query,
	}, &data)
	if err != nil {
		return nil, err
	}
	return data.Search.Nodes, nil
}

func updateSummaryIssue(opts *options, i issue) error {
	fmt.Printf("updating summary issue %q %s\n", i.title, i.id)

	body, err := generateIssueSummary(opts, i)
	if err != nil {
		return err
	}

	return graphql(`
		mutation UpdateIssue ($id: String!, $body: String!) {
			updateIssue(input: {
				id: $id,
				body: $body
			}) {
				clientMutationId
			}
		}
	`, map[string]interface{}{
		"id":   i.id,
		"body": body,
	}, nil)
}

func generateIssueSummary(opts *options, si issue) (string, error) {
	issues, err := summarizedIssues(opts.user, si)
	if err != nil {
		return "", err
	}

	issuesWithMatchingLabels := "issues with matching labels"
	if qf := si.labels.queryFilter(); qf != "" {
		query := fmt.Sprintf("type:issue user:%s %s", opts.user, qf)
		issuesWithMatchingLabels = fmt.Sprintf("[%s](%s)", issuesWithMatchingLabels, searchURL(query))
	}

	content := false
	body := &strings.Builder{}
	if re := opts.summaryCommentRegex.String(); re == "" {
		fmt.Fprintf(body, "_This is generated from the newest comment on %s._\n", issuesWithMatchingLabels)
	} else {
		fmt.Fprintf(body, "_This is generated from the newest comment that matches the regular expression `%s` on %s._\n", re, issuesWithMatchingLabels)
	}

	for _, i := range issues {
		if i.ID == si.id {
			continue
		}
		content = true
		fmt.Fprintf(body, "## [%s](%s)\n", i.Title, i.URL)
		if c := i.Comments.Nodes.lastMatch(opts.summaryCommentRegex); c != nil {
			fmt.Fprintln(body, replaceHeadings(c.Body))
			fmt.Fprintf(body, "\n\n_Updated %s by @%s_\n\n", c.UpdatedAt.Format("2006-01-02 15:04:05 MST"), c.Author.Login)
		} else {
			fmt.Fprintln(body, "_No update_")
		}
	}

	if !content {
		fmt.Fprintf(body, "\nNo matching issues.\n")
	}

	return body.String(), nil
}

var h1 = regexp.MustCompile(`(?m)^\w*#([^#]*)$`)
var h2 = regexp.MustCompile(`(?m)^\w*##([^#]*)$`)

// replaceHeadings replaces h1 and h2 headings with h3 headings so the summary issue formatting looks nice.
func replaceHeadings(s string) string {
	s = h2.ReplaceAllString(s, "###$1")
	s = h1.ReplaceAllString(s, "###$1")
	return s
}

func summarizedIssues(user string, si issue) ([]*graphqlIssue, error) {
	qf := si.labels.queryFilter()
	if qf == "" {
		return nil, nil
	}
	query := fmt.Sprintf("user:%s %s", user, qf)
	return searchIssues(query)
}

func searchIssues(query string) ([]*graphqlIssue, error) {
	fmt.Printf("searching issues %s\n", query)

	data := struct {
		Search struct {
			Nodes []*graphqlIssue `json:"nodes"`
		} `json:"search"`
	}{}
	err := graphql(`
		query SearchIssues ($query: String!) {
			search(type: ISSUE, first: 100, query: $query) {
				nodes {
					... on Issue {
						id
						url
						title
						body
						author {
							login
						}
						comments(last: 100) {
							nodes {
								author {
									login
								}
								body
								updatedAt
							}
						}
					}
				}
			}
		}
	`, map[string]interface{}{
		"query": query,
	}, &data)
	if err != nil {
		return nil, err
	}
	return data.Search.Nodes, nil
}

func graphql(query string, variables map[string]interface{}, responseData interface{}) error {
	reqbody, err := json.Marshal(map[string]interface{}{
		"query":     query,
		"variables": variables,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal query %s and variables %s: %w", query, variables, err)
	}

	req, err := http.NewRequest(http.MethodPost, os.Getenv("GITHUB_GRAPHQL_URL"), bytes.NewBuffer(reqbody))
	if err != nil {
		return err
	}

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return fmt.Errorf("empty GITHUB_TOKEN")
	}
	req.Header.Set("Authorization", "bearer "+token)

	reqdump, err := httputil.DumpRequestOut(req, true)
	if err != nil {
		return fmt.Errorf("error dumping request: %w", err)
	}

	cl := &http.Client{}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respdump, err := httputil.DumpResponse(resp, true)
	if err != nil {
		return fmt.Errorf("error dumping response: %w", err)
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("non-200 response:\n%s\n\nrequest:\n%s", string(respdump), string(reqdump))
	}

	response := struct {
		Data   interface{}
		Errors []struct {
			Type    string   `json:"type"`
			Path    []string `json:"path"`
			Message string   `json:"message"`
		} `json:"errors"`
	}{
		Data: responseData,
	}

	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return fmt.Errorf("error decoding json response:\n%s\n%w", respdump, err)
	}

	if len(response.Errors) > 0 {
		return fmt.Errorf("graphql error: %s\nrequest:\n%s", response.Errors[0].Message, reqdump)
	}

	// fmt.Fprintln(verbose, string(reqdump))
	// fmt.Fprintln(verbose, string(respdump))

	return nil
}

type options struct {
	user                string
	event               event
	summaryCommentRegex *regexp.Regexp
}

type event struct {
	Name    string
	Changes struct {
		Body struct {
			From string `json:"from"`
		} `json:"body"`
	} `json:"changes"`
	Action  string     `json:"action"`
	Issue   *restIssue `json:"issue"`
	Label   *label     `json:"label"`
	Comment *comment   `json:"comment"`
}

type restIssue struct {
	ID     string `json:"node_id"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Author actor  `json:"user"`
	Labels labels `json:"labels"`
}

type graphqlIssue struct {
	ID     string `json:"id"`
	URL    string `json:"url"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Author actor  `json:"author"`
	Labels struct {
		Nodes labels `json:"nodes"`
	} `json:"labels"`
	Comments struct {
		Nodes comments `json:"nodes"`
	} `json:"comments"`
}

type label struct {
	Name string `json:"name"`
}

type labels []label

func (l labels) contains(s string) bool {
	for _, n := range l {
		if n.Name == s {
			return true
		}
	}
	return false
}

func (l labels) nonSummaryLabels() labels {
	ls := labels{}
	for _, label := range l {
		if label.Name != "summary" {
			ls = append(ls, label)
		}
	}
	return ls
}

func (l labels) queryFilter() string {
	ls := []string{}
	for _, label := range l.nonSummaryLabels() {
		ls = append(ls, fmt.Sprintf("%q", label.Name))
	}
	if len(ls) == 0 {
		return ""
	}
	return "label:" + strings.Join(ls, ",")
}

type comments []*comment

func (c comments) lastMatch(r *regexp.Regexp) *comment {
	for j := len(c) - 1; j >= 0; j-- {
		if r.MatchString(c[j].Body) {
			return c[j]
		}
	}
	return nil
}

type comment struct {
	Author    actor     `json:"author"`
	Body      string    `json:"body"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type actor struct {
	Login string `json:"login"`
}

type issue struct {
	id     string
	title  string
	labels labels
}

func isany(s string, v ...string) bool {
	for _, x := range v {
		if s == x {
			return true
		}
	}
	return false
}

func searchURL(query string) string {
	q := url.Values{}
	q.Set("q", query)
	return os.Getenv("GITHUB_SERVER_URL") + "/search?" + q.Encode()
}
