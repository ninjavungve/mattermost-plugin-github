package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/google/go-github/github"
	"github.com/mattermost/mattermost-server/model"
	"github.com/mattermost/mattermost-server/plugin"
	"golang.org/x/oauth2"
)

const (
	GITHUB_TOKEN_KEY = "_githubtoken"
)

type Plugin struct {
	api           plugin.API
	configuration atomic.Value
	githubClient  *github.Client
	userId        string
}

func githubConnect(token string) *github.Client {
	ctx := context.Background()
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)
	tc := oauth2.NewClient(ctx, ts)

	client := github.NewClient(tc)

	return client
}

func (p *Plugin) OnActivate(api plugin.API) error {
	p.api = api
	if err := p.OnConfigurationChange(); err != nil {
		return err
	}

	config := p.config()
	if err := config.IsValid(); err != nil {
		return err
	}

	// Connect to github
	p.githubClient = githubConnect(config.GithubToken)

	// Register commands
	p.api.RegisterCommand(&model.Command{
		Trigger:     "github",
		DisplayName: "Github",
		Description: "Integration with Github.",
	})

	// Get our userId
	user, err := p.api.GetUserByUsername(config.Username)
	if err != nil {
		return err
	}

	p.userId = user.Id

	return nil
}

func (p *Plugin) ExecuteCommand(args *model.CommandArgs) (*model.CommandResponse, *model.AppError) {
	config := p.config()
	split := strings.Split(args.Command, " ")
	command := split[0]
	parameters := []string{}
	action := ""
	if len(split) > 1 {
		action = split[1]
	}
	if len(split) > 2 {
		parameters = split[2:]
	}

	if command != "/github" {
		return nil, nil
	}

	switch action {
	case "subscribe":
		if len(parameters) != 1 {
			return &model.CommandResponse{Text: "Wrong number of parameters.", ResponseType: model.COMMAND_RESPONSE_TYPE_EPHEMERAL}, nil
		}
		subscriptions, _ := NewSubscriptionsFromKVStore(p.api.KeyValueStore())

		subscriptions.Add(args.ChannelId, parameters[0])

		subscriptions.StoreInKVStore(p.api.KeyValueStore())

		resp := &model.CommandResponse{
			ResponseType: model.COMMAND_RESPONSE_TYPE_IN_CHANNEL,
			Text:         "You have subscribed to the repository.",
			Username:     "github",
			IconURL:      "https://assets-cdn.github.com/images/modules/logos_page/GitHub-Mark.png",
			Type:         model.POST_DEFAULT,
		}
		return resp, nil
	case "register":
		if len(parameters) != 1 {
			return &model.CommandResponse{Text: "Wrong number of parameters.", ResponseType: model.COMMAND_RESPONSE_TYPE_EPHEMERAL}, nil
		}
		p.api.KeyValueStore().Set(args.UserId+GITHUB_TOKEN_KEY, []byte(parameters[0]))
		resp := &model.CommandResponse{
			ResponseType: model.COMMAND_RESPONSE_TYPE_EPHEMERAL,
			Text:         "Registered github token.",
			Username:     "github",
			IconURL:      "https://assets-cdn.github.com/images/modules/logos_page/GitHub-Mark.png",
			Type:         model.POST_DEFAULT,
		}
		return resp, nil
	case "deregister":
		p.api.KeyValueStore().Delete(args.UserId + GITHUB_TOKEN_KEY)
		resp := &model.CommandResponse{
			ResponseType: model.COMMAND_RESPONSE_TYPE_EPHEMERAL,
			Text:         "Deregistered github token.",
			Username:     "github",
			IconURL:      "https://assets-cdn.github.com/images/modules/logos_page/GitHub-Mark.png",
			Type:         model.POST_DEFAULT,
		}
		return resp, nil
	case "todo":
		go p.HandleTodo(args.UserId, config.GithubOrg)
		return &model.CommandResponse{Text: "Checking GitHub for your pending PRs reviews. Get a :coffee:", ResponseType: model.COMMAND_RESPONSE_TYPE_EPHEMERAL}, nil
	}

	return nil, nil
}

func (p *Plugin) config() *Configuration {
	return p.configuration.Load().(*Configuration)
}

func (p *Plugin) OnConfigurationChange() error {
	var configuration Configuration
	err := p.api.LoadPluginConfiguration(&configuration)
	p.configuration.Store(&configuration)
	return err
}

func (p *Plugin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	config := p.config()
	if err := config.IsValid(); err != nil {
		http.Error(w, "This plugin is not configured.", http.StatusNotImplemented)
		return
	}

	switch path := r.URL.Path; path {
	case "/webhook":
		p.handleWebhook(w, r)
	case "/api/v1/pr/reviewers":
		p.handleReviewers(w, r)
	default:
		http.NotFound(w, r)
	}
}

type PullRequestWaitingReview struct {
	GitHubRepo        string `url:"github_repo"`
	GitHubUserName    string `url:"github_username"`
	PullRequestNumber int    `url:"pullrequest_number"`
	PullRequestURL    string `url:"pullrequest_url"`
}

type PullRequestWaitingReviews []PullRequestWaitingReview

func (p *Plugin) HandleTodo(userId, gitHubOrg string) {
	ctx := context.Background()

	dmChannel, err := p.api.GetDirectChannel(userId, userId)
	if err != nil {
		fmt.Println("Error to get the DM channel")
		return
	}

	b, err := p.api.KeyValueStore().Get(userId + GITHUB_TOKEN_KEY)
	if err != nil {
		p.SendTodoPost("Error retrieving the GitHub User token", p.userId, dmChannel.Id)
	}
	gitHubUserToken := string(b)

	githubClient := githubConnect(gitHubUserToken)

	// Get the user information. We need to know the username
	me, _, err2 := githubClient.Users.Get(ctx, "")
	if err2 != nil {
		p.SendTodoPost("Error retrieving the GitHub User information", p.userId, dmChannel.Id)
	}

	// Get all repositories for one specific Organization and after that get an PRs for
	// each repository that are waiting review from the user.
	var repos []string
	githubRepos, _, err2 := githubClient.Repositories.ListByOrg(ctx, gitHubOrg, nil)
	if err2 != nil {
		p.SendTodoPost("Error retrieving the GitHub repository", p.userId, dmChannel.Id)
	}
	for _, repo := range githubRepos {
		repos = append(repos, repo.GetName())
	}

	var prWaitingReviews PullRequestWaitingReviews
	for _, repo := range repos {
		prs, _, err := githubClient.PullRequests.List(ctx, gitHubOrg, repo, nil)
		if err != nil {
			p.SendTodoPost("Error retrieving the GitHub PRs List", p.userId, dmChannel.Id)
		}
		for _, pull := range prs {
			prReviewers, _, err := githubClient.PullRequests.ListReviewers(ctx, gitHubOrg, repo, pull.GetNumber(), nil)
			if err != nil {
				p.SendTodoPost("Error retrieving the GitHub PRs Reviewers", p.userId, dmChannel.Id)
			}
			for _, reviewer := range prReviewers.Users {
				if reviewer.GetLogin() == me.GetLogin() {
					prWaitingReviews = append(prWaitingReviews, PullRequestWaitingReview{repo, reviewer.GetLogin(), pull.GetNumber(), pull.GetHTMLURL()})
				}
			}
		}
	}

	if len(prWaitingReviews) != 0 {
		var buffer bytes.Buffer
		for _, toReview := range prWaitingReviews {
			buffer.WriteString(fmt.Sprintf("[**%v**] PRs waiting %v's review: **PR-%v** url: %v\n", toReview.GitHubRepo, toReview.GitHubUserName, toReview.PullRequestNumber, toReview.PullRequestURL))
		}
		p.SendTodoPost(buffer.String(), p.userId, dmChannel.Id)
	} else {
		p.SendTodoPost("No pending PRs to review. Go and grab a coffee :smile:", p.userId, dmChannel.Id)
	}
}

func (p *Plugin) SendTodoPost(message, userId, channelId string) {
	props := map[string]interface{}{}

	post := &model.Post{
		UserId:    userId,
		ChannelId: channelId,
		Message:   message,
		Type:      model.POST_DEFAULT,
		Props:     props,
	}
	p.api.CreatePost(post)
}

func NewString(st string) *string {
	return &st
}

func githubUserListToUsernames(users []*github.User) *[]string {
	var output []string
	for _, user := range users {
		output = append(output, *user.Login)
	}
	return &output
}

func processLables(labels []*github.Label) *[]map[string]string {
	var output []map[string]string
	for _, label := range labels {
		entry := map[string]string{
			"text":  *label.Name,
			"color": *label.Color,
		}
		output = append(output, entry)
	}

	return &output
}

func (p *Plugin) postFromPullRequest(org, repository string, pullRequest *github.PullRequest) *model.Post {
	props := map[string]interface{}{}
	props["number"] = fmt.Sprint(*pullRequest.Number)
	props["summary"] = pullRequest.Body
	props["title"] = pullRequest.Title
	props["assignees"] = githubUserListToUsernames(pullRequest.Assignees)
	prReviewers, _, _ := p.githubClient.PullRequests.ListReviewers(context.Background(), org, repository, pullRequest.GetNumber(), nil)
	props["reviewers"] = githubUserListToUsernames(prReviewers.Users)
	//labels, _, _ := p.githubClient.Issues.ListLabelsByIssue(context.Background(), org, repository, pullRequest.GetNumber(), nil)
	//props["labels"] = processLables(labels)
	props["submitted_at"] = fmt.Sprint(pullRequest.CreatedAt.Unix())

	return &model.Post{
		UserId:  p.userId,
		Message: "Joram screwed up",
		Type:    "custom_github_pull_request",
		Props:   props,
	}
}

func (p *Plugin) handleWebhook(w http.ResponseWriter, r *http.Request) {
	config := p.config()

	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("secret")), []byte(config.WebhookSecret)) != 1 {
		http.Error(w, "Not authorized", http.StatusUnauthorized)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request body", http.StatusBadRequest)
		return
	}

	/*payload, err := github.ValidatePayload(r, []byte(config.WebhookSecret))
	if err != nil {
		fmt.Println("Err: " + err.Error())
	}*/
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		fmt.Println("Err: " + err.Error())
	}
	event, err := github.ParseWebHook(github.WebHookType(r), body)
	if err != nil {
		fmt.Println("Err2: " + err.Error())
	}
	switch event := event.(type) {
	case *github.PullRequestEvent:
		fmt.Println("Stufff")
		fmt.Println(*event)
		fmt.Println(*event.Repo)
		p.pullRequestOpened(event.GetRepo().GetFullName(), event.PullRequest)
	}
}

func (p *Plugin) pullRequestOpened(repo string, pullRequest *github.PullRequest) {
	subscriptions, err := NewSubscriptionsFromKVStore(p.api.KeyValueStore())
	if err != nil {
		fmt.Println("Error: " + err.Error())
	}
	fmt.Println("Subscriptions:")
	fmt.Println(*subscriptions)
	fmt.Println("Repo: " + repo)

	gob.Register([]map[string]string{})

	channels := subscriptions.GetChannelsForRepository(repo)
	values := strings.Split(repo, "/")
	post := p.postFromPullRequest(values[0], values[1], pullRequest)
	for _, channel := range channels {
		post.ChannelId = channel
		_, err := p.api.CreatePost(post)
		fmt.Println("Chan: " + channel)
		if err != nil {
			fmt.Println("Chanerr: " + err.Error())
		}
	}
}

type AddReviewersToPR struct {
	PullRequestId int      `json:"pull_request_id"`
	Org           string   `json:"org"`
	Repo          string   `json:"repo"`
	Reviewers     []string `json:"reviewers"`
}

func (p *Plugin) handleReviewers(w http.ResponseWriter, r *http.Request) {
	ctx := context.Background()
	var req AddReviewersToPR
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	userId := r.Header.Get("Mattermost-User-Id")
	if userId == "" {
		http.Error(w, "Not authorized", http.StatusUnauthorized)
		return
	}

	b, err := p.api.KeyValueStore().Get(userId + GITHUB_TOKEN_KEY)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	gitHubUserToken := string(b)

	githubClient := githubConnect(gitHubUserToken)

	reviewers := github.ReviewersRequest{
		Reviewers: req.Reviewers,
	}

	pr, _, err2 := githubClient.PullRequests.RequestReviewers(ctx, req.Org, req.Repo, req.PullRequestId, reviewers)
	if err2 != nil {
		http.Error(w, err2.Error(), http.StatusBadRequest)
		return
	}

	w.Write([]byte(fmt.Sprintf("%v", pr.GetHTMLURL())))
}
