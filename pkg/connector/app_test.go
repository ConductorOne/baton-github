package connector

import (
	"context"
	"testing"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	resourceSdk "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/google/go-github/v69/github"
	"github.com/stretchr/testify/require"
)

func TestAppResource(t *testing.T) {
	ctx := context.Background()

	installation := &github.Installation{
		ID:                  github.Ptr(int64(42)),
		AppID:               github.Ptr(int64(7)),
		AppSlug:             github.Ptr("octo-app"),
		TargetType:          github.Ptr("Organization"),
		RepositorySelection: github.Ptr("all"),
		HTMLURL:             github.Ptr("https://github.com/organizations/acme/settings/installations/42"),
		Account:             &github.User{Login: github.Ptr("acme")},
	}
	parent := &v2.ResourceId{ResourceType: resourceTypeOrg.Id, Resource: "1"}

	resource, err := appResource(ctx, installation, parent)
	require.NoError(t, err)
	require.Equal(t, "octo-app", resource.DisplayName)
	require.Equal(t, "42", resource.Id.Resource)
	require.Equal(t, resourceTypeApp.Id, resource.Id.ResourceType)
	require.Equal(t, parent.Resource, resource.ParentResourceId.Resource)

	nhi, err := resourceSdk.GetNonHumanIdentityTrait(resource)
	require.NoError(t, err)
	require.Equal(t, v2.NonHumanIdentityTrait_NHI_TYPE_APP_REGISTRATION, nhi.GetNhiType())
	require.Equal(t, "github.app", nhi.GetNhiDetail())

	_, err = resourceSdk.GetAppTrait(resource)
	require.NoError(t, err)
	// profile is now a Resource-level attribute; read it via GetProfile.
	profile := resourceSdk.GetProfile(resource)
	appID, ok := resourceSdk.GetProfileInt64Value(profile, "app_id")
	require.True(t, ok)
	require.Equal(t, int64(7), appID)
	login, ok := resourceSdk.GetProfileStringValue(profile, "account_login")
	require.True(t, ok)
	require.Equal(t, "acme", login)
}
