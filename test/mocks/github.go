package mocks

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	mapset "github.com/deckarep/golang-set/v2"
	"github.com/google/go-github/v63/github"
	"github.com/migueleliasweb/go-github-mock/src/mock"
)

type MockGitHub struct {
	teamMemberships         map[int64]mapset.Set[int64]
	repositoryMemberships   map[int64]mapset.Set[int64]
	organizationMemberships map[int64]mapset.Set[int64]
	organizations           map[int64]github.Organization
	repositories            map[int64]github.Repository
	teams                   map[int64]github.Team
	users                   map[int64]github.User
}

func NewMockGitHub() *MockGitHub {
	return &MockGitHub{
		teamMemberships:         map[int64]mapset.Set[int64]{},
		repositoryMemberships:   map[int64]mapset.Set[int64]{},
		organizationMemberships: map[int64]mapset.Set[int64]{},
		organizations:           map[int64]github.Organization{},
		repositories:            map[int64]github.Repository{},
		teams:                   map[int64]github.Team{},
		users:                   map[int64]github.User{},
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

func parseBodyVariables(request *http.Request) map[string]string {
	output := make(map[string]string)

	data, _ := io.ReadAll(request.Body)
	result := make(map[string]interface{})
	err := json.Unmarshal(data, &result)
	if err != nil {
		return nil
	}
	for key, value := range result {
		switch castedValue := value.(type) {
		case string:
			output[key] = castedValue
		case float64:
			output[key] = strconv.Itoa(int(castedValue))
		default:
			// Skip other types.
			continue
		}
	}
	return output
}

func combineMaps(input ...map[string]string) map[string]string {
	output := make(map[string]string)
	for _, inputMap := range input {
		for key, value := range inputMap {
			output[key] = value
		}
	}
	return output
}

// Seed sets up the mock database with a user that is part of a team that is in an organization.
func (mgh MockGitHub) Seed() (
	*github.Organization,
	*github.Repository,
	*github.Team,
	*github.User,
	error,
) {
	organizationId := int64(12)
	repositoryId := int64(34)
	userId := int64(56)
	teamId := int64(78)

	userIdStr := strconv.FormatInt(userId, 10)
	email := fmt.Sprintf("%s@example.com", userIdStr)
	organizationName := fmt.Sprintf("organization #%d", organizationId)
	organizationSlug := fmt.Sprintf("organization-%d", organizationId)
	repositoryName := fmt.Sprintf("repository-%d", repositoryId)

	githubOrganization := github.Organization{
		ID:    &organizationId,
		Name:  &organizationName,
		Login: &organizationSlug,
	}
	githubUser := github.User{
		ID:    &userId,
		Login: &userIdStr,
		Email: &email,
		Permissions: map[string]bool{
			"permission0": true,
		},
	}
	githubRepository := github.Repository{
		ID:           &repositoryId,
		Owner:        &githubUser,
		Name:         &repositoryName,
		Organization: &githubOrganization,
	}
	githubTeam := github.Team{
		ID:           &teamId,
		Organization: &githubOrganization,
	}

	mgh.organizations[organizationId] = githubOrganization
	mgh.repositories[repositoryId] = githubRepository
	mgh.teams[teamId] = githubTeam
	mgh.users[userId] = githubUser
	mgh.teamMemberships[teamId] = mapset.NewSet[int64](userId)
	mgh.organizationMemberships[organizationId] = mapset.NewSet[int64](userId)

	return &githubOrganization, &githubRepository, &githubTeam, &githubUser, nil
}

func getResource[T interface{}](
	w http.ResponseWriter,
	idStr string,
	table map[int64]T,
) (*T, error) {
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

func (mgh MockGitHub) getUsers(
	w http.ResponseWriter,
	variables map[string]string,
) {
	mgh.getUsersFromCrossTable(
		w,
		variables,
		mgh.organizationMemberships,
		"org",
	)
}

func (mgh MockGitHub) getUser(
	w http.ResponseWriter,
	variables map[string]string,
) {
	if id, ok := variables["id"]; ok {
		writeResource(w, id, mgh.users)
	}
}

func (mgh MockGitHub) addUser(
	w http.ResponseWriter,
	variables map[string]string,
) {
	mgh.addUserToCrossTable(
		w,
		variables,
		mgh.organizationMemberships,
		"org",
	)
}

func (mgh MockGitHub) removeUser(
	w http.ResponseWriter,
	variables map[string]string,
) {
	mgh.removeUserFromCrossTable(
		w,
		variables,
		mgh.organizationMemberships,
		"org",
	)
}

func (mgh MockGitHub) getOrganization(
	w http.ResponseWriter,
	variables map[string]string,
) {
	if id, ok := variables["org_id"]; ok {
		writeResource(w, id, mgh.organizations)
	}
}

func (mgh MockGitHub) getRepository(
	w http.ResponseWriter,
	variables map[string]string,
) {
	if id, ok := variables["repository_id"]; ok {
		writeResource(w, id, mgh.repositories)
	}
}

func (mgh MockGitHub) getRepositoryTeams(
	w http.ResponseWriter,
	variables map[string]string,
) {
	// TODO(marcos): Implement granting repositories to teams.
}

func (mgh MockGitHub) getTeam(
	w http.ResponseWriter,
	variables map[string]string,
) {
	if id, ok := variables["team_id"]; ok {
		writeResource(w, id, mgh.teams)
	}
}

func (mgh MockGitHub) getUsersFromCrossTable(
	w http.ResponseWriter,
	variables map[string]string,
	crossTable map[int64]mapset.Set[int64],
	crossTableKey string,
) {
	crossTableId, _ := getCrossTableId(w, variables, crossTableKey)
	memberships, ok := crossTable[crossTableId]
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

func (mgh MockGitHub) getUserFromCrossTable(
	w http.ResponseWriter,
	variables map[string]string,
	crossTable map[int64]mapset.Set[int64],
	crossTableKey string,
) *github.User {
	crossTableId, _ := getCrossTableId(w, variables, crossTableKey)
	userId, _ := getUserId(w, variables)

	memberships, ok := crossTable[crossTableId]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return nil
	}

	if !memberships.Contains(userId) {
		w.WriteHeader(http.StatusNotFound)
		return nil
	}

	user, ok := mgh.users[userId]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return nil
	}

	return &user
}

func getCrossTableId(
	w http.ResponseWriter,
	variables map[string]string,
	crossTableKey string,
) (int64, error) {
	crossTableId, ok := variables[crossTableKey]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return 0, fmt.Errorf("crossTableKey not found")
	}

	// HACK: pretend IDs and slugs are the same.
	crossTableId = regexp.MustCompile(`[^0-9]+`).ReplaceAllString(crossTableId, "")

	id, err := strconv.ParseInt(crossTableId, 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return 0, fmt.Errorf("crossTableId malformed")
	}
	return id, nil
}

// getVariable looks up a variable in a map, with the ability to fall back with
// other keys if the value isn't immediately found.
func getVariable(variables map[string]string, keys ...string) (string, bool) {
	for _, key := range keys {
		found, ok := variables[key]
		if ok {
			return found, true
		}
	}
	return "", false
}

func getUserId(
	w http.ResponseWriter,
	variables map[string]string,
) (int64, error) {
	username, ok := getVariable(variables, "username", "invitee_id")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return 0, fmt.Errorf("username not set")
	}
	userId, err := strconv.ParseInt(username, 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return 0, fmt.Errorf("username malformed")
	}
	return userId, nil
}

func (mgh MockGitHub) addUserToCrossTable(
	w http.ResponseWriter,
	variables map[string]string,
	crossTable map[int64]mapset.Set[int64],
	crossTableKey string,
) {
	crossTableId, _ := getCrossTableId(w, variables, crossTableKey)
	userId, _ := getUserId(w, variables)
	if _, ok := crossTable[crossTableId]; ok {
		crossTable[crossTableId].Add(userId)
	} else {
		crossTable[crossTableId] = mapset.NewSet[int64](userId)
	}
}

func (mgh MockGitHub) removeUserFromCrossTable(
	w http.ResponseWriter,
	variables map[string]string,
	crossTable map[int64]mapset.Set[int64],
	crossTableKey string,
) {
	crossTableId, _ := getCrossTableId(w, variables, crossTableKey)
	userId, _ := getUserId(w, variables)

	crossTable[crossTableId].Remove(userId)
}

func (mgh MockGitHub) getMembers(
	w http.ResponseWriter,
	variables map[string]string,
) {
	mgh.getUsersFromCrossTable(
		w,
		variables,
		mgh.teamMemberships,
		"team_id",
	)
}

func userToMembership(user *github.User) github.Membership {
	return github.Membership{
		User:  user,
		Role:  github.String("admin"),
		State: github.String("active"),
	}
}

func (mgh MockGitHub) getMembership(
	w http.ResponseWriter,
	variables map[string]string,
) {
	userId, _ := getUserId(w, variables)
	if user, ok := mgh.users[userId]; ok {
		_, _ = w.Write(mock.MustMarshal(userToMembership(&user)))
	}
}

func (mgh MockGitHub) getTeamMembership(
	w http.ResponseWriter,
	variables map[string]string,
) {
	user := mgh.getUserFromCrossTable(
		w,
		variables,
		mgh.teamMemberships,
		"team_id",
	)
	_, _ = w.Write(mock.MustMarshal(userToMembership(user)))
}

func (mgh MockGitHub) getRepositoryCollaborators(
	w http.ResponseWriter,
	variables map[string]string,
) {
	mgh.getUsersFromCrossTable(
		w,
		variables,
		mgh.repositoryMemberships,
		"repo",
	)
}

func (mgh MockGitHub) getRepositoryCollaborator(
	w http.ResponseWriter,
	variables map[string]string,
) {
	user := mgh.getUserFromCrossTable(
		w,
		variables,
		mgh.repositoryMemberships,
		"repository_id",
	)
	_, _ = w.Write(
		mock.MustMarshal(user),
	)
}

func (mgh MockGitHub) addMembership(
	w http.ResponseWriter,
	variables map[string]string,
) {
	mgh.addUserToCrossTable(
		w,
		variables,
		mgh.teamMemberships,
		"team_id",
	)
}

func (mgh MockGitHub) addRepositoryCollaborator(
	w http.ResponseWriter,
	variables map[string]string,
) {
	mgh.addUserToCrossTable(
		w,
		variables,
		mgh.repositoryMemberships,
		"repo",
	)
}

func (mgh MockGitHub) removeMembership(
	w http.ResponseWriter,
	variables map[string]string,
) {
	mgh.removeUserFromCrossTable(
		w,
		variables,
		mgh.teamMemberships,
		"team_id",
	)
}

func (mgh MockGitHub) removeRepositoryCollaborator(
	w http.ResponseWriter,
	variables map[string]string,
) {
	mgh.removeUserFromCrossTable(
		w,
		variables,
		mgh.repositoryMemberships,
		"repo",
	)
}

type handler = func(w http.ResponseWriter, variables map[string]string)

// addEndpointHandler takes a string interpolation pattern and a handler
// function and returns a route definition.
func addEndpointHandler(
	endpoint mock.EndpointPattern,
	function handler,
) mock.MockBackendOption {
	return mock.WithRequestMatchHandler(
		endpoint,
		http.HandlerFunc(
			func(w http.ResponseWriter, request *http.Request) {
				function(
					w,
					combineMaps(
						parseUrlVariables(
							endpoint.Pattern,
							request.URL.String(),
						),
						parseBodyVariables(request),
					),
				)
			},
		),
	)
}

func (mgh MockGitHub) Server() *http.Client {
	routesMap := map[mock.EndpointPattern]handler{
		GetOrganizationById:                                                 mgh.getOrganization,
		GetOrganizationsTeamsMembersByTeamId:                                mgh.getMembers,
		GetOrganizationsTeamByTeamId:                                        mgh.getTeam,
		GetOrganizationsTeamsMembershipsByTeamIdByUsername:                  mgh.getTeamMembership,
		GetRepositoryById:                                                   mgh.getRepository,
		GetUserById:                                                         mgh.getUser,
		mock.DeleteOrgsMembershipsByOrgByUsername:                           mgh.removeUser,
		mock.DeleteReposCollaboratorsByOwnerByRepoByUsername:                mgh.removeRepositoryCollaborator,
		mock.GetOrgsMembersByOrg:                                            mgh.getUsers,
		mock.GetOrgsMembershipsByOrgByUsername:                              mgh.getMembership,
		mock.GetReposCollaboratorsByOwnerByRepo:                             mgh.getRepositoryCollaborators,
		mock.GetReposCollaboratorsByOwnerByRepoByUsername:                   mgh.getRepositoryCollaborator,
		mock.GetReposTeamsByOwnerByRepo:                                     mgh.getRepositoryTeams,
		mock.PostOrgsInvitationsByOrg:                                       mgh.addUser,
		mock.PutReposCollaboratorsByOwnerByRepoByUsername:                   mgh.addRepositoryCollaborator,
		DeleteOrganizationsTeamsMembershipsByOrganizationByTeamIdByUsername: mgh.removeMembership,
		PutOrganizationsTeamsMembershipsByOrganizationByTeamIdByUsername:    mgh.addMembership,
	}

	options := make([]mock.MockBackendOption, 0)
	for key, value := range routesMap {
		options = append(options, addEndpointHandler(key, value))
	}
	return mock.NewMockedHTTPClient(options...)
}
