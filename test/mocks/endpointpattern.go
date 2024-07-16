package mocks

import "github.com/migueleliasweb/go-github-mock/src/mock"

var GetUserById = mock.EndpointPattern{
	Pattern: "/user/{id}",
	Method:  "GET",
}

var PutOrganizationsTeamsMembershipsByOrganizationByTeamIdByUsername = mock.EndpointPattern{
	Pattern: "/organizations/{org_id}/team/{team_id}/memberships/{username}",
	Method:  "PUT",
}

var DeleteOrganizationsTeamsMembershipsByOrganizationByTeamIdByUsername = mock.EndpointPattern{
	Pattern: "/organizations/{org_id}/team/{team_id}/memberships/{username}",
	Method:  "DELETE",
}

var GetOrganizationById = mock.EndpointPattern{
	Pattern: "/organizations/{org_id}",
	Method:  "GET",
}

var GetRepositoryById = mock.EndpointPattern{
	Pattern: "/repositories/{repository_id}",
	Method:  "GET",
}

var GetOrganizationsTeamsMembersByTeamId = mock.EndpointPattern{
	Pattern: "/organizations/{org_id}/team/{team_id}/members",
	Method:  "GET",
}

var GetOrganizationsTeamsMembershipsByTeamIdByUsername = mock.EndpointPattern{
	Pattern: "/organizations/{org_id}/team/{team_id}/memberships/{username}",
	Method:  "GET",
}
