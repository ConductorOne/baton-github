package customclient

// https://docs.github.com/en/enterprise-cloud@latest/rest/enterprise-admin/license?apiVersion=2022-11-28#list-enterprise-consumed-licenses
type EnterpriseConsumedLicense struct {
	TotalSeatsConsumed  int                    `json:"total_seats_consumed"`
	TotalSeatsPurchased int                    `json:"total_seats_purchased"`
	Users               []GitHubEnterpriseUser `json:"users"`
}

type GitHubEnterpriseUser struct {
	GitHubComLogin                       string   `json:"github_com_login"`
	GitHubComName                        string   `json:"github_com_name"`
	EnterpriseServerUserIDs              []string `json:"enterprise_server_user_ids"`
	GitHubComUser                        bool     `json:"github_com_user"`
	EnterpriseServerUser                 bool     `json:"enterprise_server_user"`
	VisualStudioSubscriptionUser         bool     `json:"visual_studio_subscription_user"`
	LicenseType                          string   `json:"license_type"`
	GitHubComProfile                     string   `json:"github_com_profile"`
	GitHubComMemberRoles                 []string `json:"github_com_member_roles"`
	GitHubComEnterpriseRoles             []string `json:"github_com_enterprise_roles"`
	GitHubComVerifiedDomainEmails        []string `json:"github_com_verified_domain_emails"`
	GitHubComSAMLNameID                  *string  `json:"github_com_saml_name_id"`
	GitHubComOrgsWithPendingInvites      []string `json:"github_com_orgs_with_pending_invites"`
	GitHubComTwoFactorAuth               bool     `json:"github_com_two_factor_auth"`
	GitHubComTwoFactorAuthRequiredByDate string   `json:"github_com_two_factor_auth_required_by_date"`
	GitHubComCostCenter                  *string  `json:"github_com_cost_center"`
	GitHubComCodeSecurityLicenseUser     bool     `json:"github_com_code_security_license_user"`
	GitHubComSecretProtectionLicenseUser bool     `json:"github_com_secret_protection_license_user"`
	GHELicenseActive                     bool     `json:"ghe_license_active"`
	GHELicenseStartDate                  *string  `json:"ghe_license_start_date"`
	GHELicenseEndDate                    string   `json:"ghe_license_end_date"`
	EnterpriseServerPrimaryEmails        []string `json:"enterprise_server_primary_emails"`
	VisualStudioLicenseStatus            *string  `json:"visual_studio_license_status"`
	VisualStudioSubscriptionEmail        *string  `json:"visual_studio_subscription_email"`
	TotalUserAccounts                    int      `json:"total_user_accounts"`
}
