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
)

var policy = map[Role]map[Permission]bool{
	RoleOwner:  {PermissionMembersRead: true, PermissionMembersInvite: true, PermissionMonitorsRead: true, PermissionMonitorsManage: true, PermissionIncidentsRead: true, PermissionIncidentsManage: true},
	RoleAdmin:  {PermissionMembersRead: true, PermissionMembersInvite: true, PermissionMonitorsRead: true, PermissionMonitorsManage: true, PermissionIncidentsRead: true, PermissionIncidentsManage: true},
	RoleMember: {PermissionMembersRead: true, PermissionMonitorsRead: true, PermissionMonitorsManage: true, PermissionIncidentsRead: true, PermissionIncidentsManage: true},
	RoleViewer: {PermissionMembersRead: true, PermissionMonitorsRead: true, PermissionIncidentsRead: true},
}

func Allows(role Role, permission Permission) bool { return policy[role][permission] }

func ValidAssignableRole(role Role) bool {
	return role == RoleAdmin || role == RoleMember || role == RoleViewer
}
