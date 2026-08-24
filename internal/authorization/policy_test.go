package authorization

import "testing"

func TestPermissionMatrix(t *testing.T) {
	tests := []struct {
		role       Role
		permission Permission
		want       bool
	}{
		{RoleOwner, PermissionMembersInvite, true}, {RoleAdmin, PermissionMembersInvite, true},
		{RoleMember, PermissionMembersInvite, false}, {RoleViewer, PermissionMembersInvite, false},
		{RoleOwner, PermissionMonitorsManage, true}, {RoleAdmin, PermissionMonitorsManage, true},
		{RoleMember, PermissionMonitorsManage, true}, {RoleViewer, PermissionMonitorsManage, false},
		{RoleOwner, PermissionIncidentsRead, true}, {RoleViewer, PermissionIncidentsRead, true},
		{RoleMember, PermissionIncidentsManage, true}, {RoleViewer, PermissionIncidentsManage, false},
	}
	for _, test := range tests {
		if got := Allows(test.role, test.permission); got != test.want {
			t.Errorf("Allows(%q, %q) = %t, want %t", test.role, test.permission, got, test.want)
		}
	}
}
