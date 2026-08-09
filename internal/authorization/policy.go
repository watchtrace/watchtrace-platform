// Package authorization defines the single backend role-permission policy.
package authorization

type Role string
type Permission string

const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
	RoleViewer Role = "viewer"

	PermissionMembersRead    Permission = "members:read"
	PermissionMembersInvite  Permission = "members:invite"
	PermissionMonitorsRead   Permission = "monitors:read"
	PermissionMonitorsManage Permission = "monitors:manage"
)

var policy = map[Role]map[Permission]bool{
	RoleOwner:  {PermissionMembersRead: true, PermissionMembersInvite: true, PermissionMonitorsRead: true, PermissionMonitorsManage: true},
	RoleAdmin:  {PermissionMembersRead: true, PermissionMembersInvite: true, PermissionMonitorsRead: true, PermissionMonitorsManage: true},
	RoleMember: {PermissionMembersRead: true, PermissionMonitorsRead: true, PermissionMonitorsManage: true},
	RoleViewer: {PermissionMembersRead: true, PermissionMonitorsRead: true},
}

func Allows(role Role, permission Permission) bool { return policy[role][permission] }

func ValidAssignableRole(role Role) bool {
	return role == RoleAdmin || role == RoleMember || role == RoleViewer
}
