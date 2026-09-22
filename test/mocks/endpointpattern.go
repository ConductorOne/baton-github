package mocks

import "github.com/migueleliasweb/go-github-mock/src/mock"

// methodGet is extracted so goconst stops flagging the repeated literal across
// the endpoint patterns below.
const methodGet = "GET"

var GetUserById = mock.EndpointPattern{
	Pattern: "/user/{id}",
	Method:  methodGet,
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
	Method:  methodGet,
}

var GetOrgsByOrg = mock.EndpointPattern{
	Pattern: "/orgs/{org}",
	Method:  methodGet,
}

var GetRepositoryById = mock.EndpointPattern{
	Pattern: "/repositories/{repository_id}",
	Method:  methodGet,
}

var GetOrganizationsTeamByTeamId = mock.EndpointPattern{
	Pattern: "/organizations/{org_id}/team/{team_id}",
	Method:  methodGet,
}

var GetOrganizationsTeamsMembersByTeamId = mock.EndpointPattern{
	Pattern: "/organizations/{org_id}/team/{team_id}/members",
	Method:  methodGet,
}

var GetOrganizationsTeamsMembershipsByTeamIdByUsername = mock.EndpointPattern{
	Pattern: "/organizations/{org_id}/team/{team_id}/memberships/{username}",
	Method:  methodGet,
}

// Organization role endpoints.
var GetOrgsRolesByOrg = mock.EndpointPattern{
	Pattern: "/orgs/{org}/organization-roles",
	Method:  methodGet,
}

var GetOrgsRolesTeamsByOrgByRoleId = mock.EndpointPattern{
	Pattern: "/orgs/{org}/organization-roles/{role_id}/teams",
	Method:  methodGet,
}

var GetOrgsRolesUsersByOrgByRoleId = mock.EndpointPattern{
	Pattern: "/orgs/{org}/organization-roles/{role_id}/users",
	Method:  methodGet,
}

var PutOrgsRolesUsersByOrgByRoleIdByUsername = mock.EndpointPattern{
	Pattern: "/orgs/{org}/organization-roles/users/{username}/{role_id}",
	Method:  "PUT",
}

var DeleteOrgsRolesUsersByOrgByRoleIdByUsername = mock.EndpointPattern{
	Pattern: "/orgs/{org}/organization-roles/users/{username}/{role_id}",
	Method:  "DELETE",
}
