// Package authorization defines the single backend role-permission policy.
package authorization

type Role string
type Permission string

const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
	RoleViewer Role = "viewer"

	PermissionMembersRead     Permission = "members:read"
	PermissionMembersInvite   Permission = "members:invite"
	PermissionMonitorsRead    Permission = "monitors:read"
	PermissionMonitorsManage  Permission = "monitors:manage"
	PermissionIncidentsRead   Permission = "incidents:read"
	PermissionIncidentsManage Permission = "incidents:manage"
	PermissionTenantRead      Permission = "tenant:read"
	PermissionTenantManage    Permission = "tenant:manage"
	PermissionMembersManage   Permission = "members:manage"
)

var policy = map[Role]map[Permission]bool{
	RoleOwner:  {PermissionMembersRead: true, PermissionMembersInvite: true, PermissionMembersManage: true, PermissionMonitorsRead: true, PermissionMonitorsManage: true, PermissionIncidentsRead: true, PermissionIncidentsManage: true, PermissionTenantRead: true, PermissionTenantManage: true},
	RoleAdmin:  {PermissionMembersRead: true, PermissionMembersInvite: true, PermissionMembersManage: true, PermissionMonitorsRead: true, PermissionMonitorsManage: true, PermissionIncidentsRead: true, PermissionIncidentsManage: true, PermissionTenantRead: true, PermissionTenantManage: true},
	RoleMember: {PermissionMembersRead: true, PermissionMonitorsRead: true, PermissionMonitorsManage: true, PermissionIncidentsRead: true, PermissionIncidentsManage: true, PermissionTenantRead: true},
	RoleViewer: {PermissionMembersRead: true, PermissionMonitorsRead: true, PermissionIncidentsRead: true, PermissionTenantRead: true},
}

func Allows(role Role, permission Permission) bool { return policy[role][permission] }

func ValidAssignableRole(role Role) bool {
	return role == RoleAdmin || role == RoleMember || role == RoleViewer
}

func AllowedActions(role Role) []string {
	order := []Permission{PermissionTenantRead, PermissionTenantManage, PermissionMembersRead,
		PermissionMembersInvite, PermissionMembersManage, PermissionMonitorsRead,
		PermissionMonitorsManage, PermissionIncidentsRead, PermissionIncidentsManage}
	actions := make([]string, 0, len(order))
	for _, permission := range order {
		if Allows(role, permission) {
			actions = append(actions, string(permission))
		}
	}
	return actions
}
