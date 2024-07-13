package mocks

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	mapset "github.com/deckarep/golang-set/v2"
	"github.com/google/go-github/v63/github"
	"github.com/migueleliasweb/go-github-mock/src/mock"
)

type MockGitHub struct {
	users         map[int64]github.User
	memberships   map[int64]mapset.Set[int64]
	organizations map[int64]github.Organization
	teams         map[int64]github.Team
}

func NewMockGitHub() *MockGitHub {
	return &MockGitHub{
		memberships:   map[int64]mapset.Set[int64]{},
		organizations: map[int64]github.Organization{},
		teams:         map[int64]github.Team{},
		users:         map[int64]github.User{},
	}
}

func parseUrlVariables(template, url string) map[string]string {
	variablesRegex := regexp.MustCompile(`{[^{}]*}`)
	output := make(map[string]string)

	urlParts := strings.Split(url, "/")
	for i, part := range strings.Split(template, "/") {
		if variablesRegex.MatchString(part) {
			key := strings.Trim(part, "{}")
			output[key] = urlParts[i]
		}
	}

	return output
}

// Seed sets up the mock database with a user that is part of a team that is in an organization.
func (mgh MockGitHub) Seed() (*github.Organization, *github.Team, *github.User, error) {
	organizationId := int64(789)
	userId := int64(123)
	teamId := int64(456)

	userIdStr := strconv.FormatInt(userId, 10)
	email := fmt.Sprintf("%s@example.com", userIdStr)

	githubOrganization := github.Organization{
		ID: &organizationId,
	}
	githubUser := github.User{
		ID:    &userId,
		Login: &userIdStr,
		Email: &email,
	}
	githubTeam := github.Team{
		ID:           &teamId,
		Organization: &githubOrganization,
	}

	mgh.users[userId] = githubUser
	mgh.teams[teamId] = githubTeam
	mgh.organizations[organizationId] = githubOrganization
	mgh.memberships[teamId] = mapset.NewSet[int64](userId)

	return &githubOrganization, &githubTeam, &githubUser, nil
}

func getResource[T interface{}](w http.ResponseWriter, idStr string, table map[int64]T) (*T, error) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return nil, err
	}
	object, ok := table[id]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return nil, err
	}

	return &object, nil
}

func writeResource[T interface{}](
	w http.ResponseWriter,
	idStr string,
	table map[int64]T,
) {
	object, err := getResource(w, idStr, table)
	if err == nil {
		_, _ = w.Write(mock.MustMarshal(object))
	}
}

func (mgh MockGitHub) getUser(
	w http.ResponseWriter,
	variables map[string]string,
) {
	if id, ok := variables["id"]; ok {
		writeResource(w, id, mgh.users)
	}
}

func (mgh MockGitHub) getOrganization(
	w http.ResponseWriter,
	variables map[string]string,
) {
	if id, ok := variables["org_id"]; ok {
		writeResource(w, id, mgh.organizations)
	}
}

func (mgh MockGitHub) getTeam(
	w http.ResponseWriter,
	variables map[string]string,
) {
	if id, ok := variables["team_id"]; ok {
		writeResource(w, id, mgh.teams)
	}
}

func (mgh MockGitHub) getMembers(
	w http.ResponseWriter,
	variables map[string]string,
) {
	teamId, ok := variables["team_id"]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	id, err := strconv.ParseInt(teamId, 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	memberships, ok := mgh.memberships[id]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	users := make([]github.User, 0)
	for _, membership := range memberships.ToSlice() {
		if user, ok := mgh.users[membership]; ok {
			users = append(users, user)
		}
	}
	_, _ = w.Write(mock.MustMarshal(users))
}

func (mgh MockGitHub) getMembership(
	w http.ResponseWriter,
	variables map[string]string,
) {
	teamId, ok := variables["team_id"]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	username, ok := variables["username"]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	id, err := strconv.ParseInt(teamId, 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	memberships, ok := mgh.memberships[id]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	userId, err := strconv.ParseInt(username, 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if !memberships.Contains(userId) {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	user, ok := mgh.users[userId]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	_, _ = w.Write(mock.MustMarshal(github.Membership{
		User: &user,
	}))
}

func (mgh MockGitHub) addMembership(
	w http.ResponseWriter,
	variables map[string]string,
) {
	teamId, ok := variables["team_id"]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	id, err := strconv.ParseInt(teamId, 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	username, ok := variables["username"]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	userId, err := strconv.ParseInt(username, 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if _, ok := mgh.memberships[id]; ok {
		mgh.memberships[id].Add(userId)
	} else {
		mgh.memberships[id] = mapset.NewSet[int64](userId)
	}
}

func (mgh MockGitHub) removeMembership(
	w http.ResponseWriter,
	variables map[string]string,
) {
	teamId, ok := variables["team_id"]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	id, err := strconv.ParseInt(teamId, 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	username, ok := variables["username"]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	userId, err := strconv.ParseInt(username, 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	mgh.memberships[id].Remove(userId)
}

func addEndpointHandler(
	endpoint mock.EndpointPattern,
	function func(
		w http.ResponseWriter,
		variables map[string]string,
	),
) mock.MockBackendOption {
	return mock.WithRequestMatchHandler(
		endpoint,
		http.HandlerFunc(
			func(w http.ResponseWriter, request *http.Request) {
				variables := parseUrlVariables(endpoint.Pattern, request.URL.String())
				function(w, variables)
			},
		),
	)
}

func (mgh MockGitHub) Server() *http.Client {
	return mock.NewMockedHTTPClient(
		addEndpointHandler(
			GetUserById,
			mgh.getUser,
		),
		addEndpointHandler(
			GetOrganizationById,
			mgh.getOrganization,
		),
		addEndpointHandler(
			GetOrganizationsTeamsMembersByTeamId,
			mgh.getMembers,
		),
		addEndpointHandler(
			GetOrganizationsTeamsMembershipsByTeamIdByUsername,
			mgh.getMembership,
		),
		addEndpointHandler(
			PutOrganizationsTeamsMembershipsByOrganizationByTeamIdByUsername,
			mgh.addMembership,
		),
		addEndpointHandler(
			DeleteOrganizationsTeamsMembershipsByOrganizationByTeamIdByUsername,
			mgh.removeMembership,
		),
	)
}
