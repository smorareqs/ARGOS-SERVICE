// Package authz gestiona la autorizacion: el catalogo fijo de permisos, los
// roles por tenant (con los siete de sistema sembrados desde la matriz del
// PRD) y las membresias usuario-tenant.
package authz

import "slices"

// Permission es un permiso del catalogo fijo (PRD seccion 3.3). Debe coincidir
// con WEB/src/app/core/auth/permissions.ts.
type Permission string

// Catalogo de permisos.
const (
	TenantSettings Permission = "tenant.settings"
	TenantBilling  Permission = "tenant.billing"

	UsersRead   Permission = "users.read"
	UsersManage Permission = "users.manage"
	RolesManage Permission = "roles.manage"

	PropertiesRead       Permission = "properties.read"
	PropertiesManage     Permission = "properties.manage"
	PropertiesAccessInfo Permission = "properties.access_info"

	TasksRead     Permission = "tasks.read"
	TasksManage   Permission = "tasks.manage"
	TasksAssign   Permission = "tasks.assign"
	TasksExecute  Permission = "tasks.execute"
	TasksValidate Permission = "tasks.validate"

	InventoryRead      Permission = "inventory.read"
	InventoryManage    Permission = "inventory.manage"
	InventoryCheckout  Permission = "inventory.checkout"
	InventoryReconcile Permission = "inventory.reconcile"

	MaintenanceRead    Permission = "maintenance.read"
	MaintenanceReport  Permission = "maintenance.report"
	MaintenanceManage  Permission = "maintenance.manage"
	MaintenanceAssign  Permission = "maintenance.assign"
	MaintenanceExecute Permission = "maintenance.execute"

	ReservationsRead    Permission = "reservations.read"
	ReservationsManage  Permission = "reservations.manage"
	ReservationsCheckin Permission = "reservations.checkin"

	GuestsRead      Permission = "guests.read"
	GuestsManage    Permission = "guests.manage"
	GuestsDocuments Permission = "guests.documents"

	DepositsRead   Permission = "deposits.read"
	DepositsManage Permission = "deposits.manage"

	ParkingRead   Permission = "parking.read"
	ParkingManage Permission = "parking.manage"

	SharedAreasManage Permission = "shared_areas.manage"
	SharedAreasBook   Permission = "shared_areas.book"
	SharedAreasRead   Permission = "shared_areas.read"

	SecurityDashboard Permission = "security.dashboard"

	HRAttendanceOwn      Permission = "hr.attendance.own"
	HRAttendanceManage   Permission = "hr.attendance.manage"
	HRPayrollManage      Permission = "hr.payroll.manage"
	HRPayrollExport      Permission = "hr.payroll.export"
	HRDisciplinaryManage Permission = "hr.disciplinary.manage"
	HRDisciplinaryOwn    Permission = "hr.disciplinary.own"
	MetricsOwn           Permission = "metrics.own"
	MetricsTeam          Permission = "metrics.team"
	MetricsAll           Permission = "metrics.all"
	AlertsRead           Permission = "alerts.read"
	AlertsManage         Permission = "alerts.manage"
	ChatInternal         Permission = "chat.internal"
	ChatGuest            Permission = "chat.guest"
	AuditRead            Permission = "audit.read"
	ReportsExport        Permission = "reports.export"
)

// All es el catalogo completo, en el orden del PRD.
var All = []Permission{
	TenantSettings, TenantBilling,
	UsersRead, UsersManage, RolesManage,
	PropertiesRead, PropertiesManage, PropertiesAccessInfo,
	TasksRead, TasksManage, TasksAssign, TasksExecute, TasksValidate,
	InventoryRead, InventoryManage, InventoryCheckout, InventoryReconcile,
	MaintenanceRead, MaintenanceReport, MaintenanceManage, MaintenanceAssign, MaintenanceExecute,
	ReservationsRead, ReservationsManage, ReservationsCheckin,
	GuestsRead, GuestsManage, GuestsDocuments,
	DepositsRead, DepositsManage,
	ParkingRead, ParkingManage,
	SharedAreasManage, SharedAreasBook, SharedAreasRead,
	SecurityDashboard,
	HRAttendanceOwn, HRAttendanceManage, HRPayrollManage, HRPayrollExport,
	HRDisciplinaryManage, HRDisciplinaryOwn,
	MetricsOwn, MetricsTeam, MetricsAll,
	AlertsRead, AlertsManage,
	ChatInternal, ChatGuest,
	AuditRead, ReportsExport,
}

// Valid indica si p pertenece al catalogo.
func (p Permission) Valid() bool {
	return slices.Contains(All, p)
}

// Feature es una bandera de funcionalidad por tenant (PRD seccion 5.1).
type Feature string

// Banderas de funcionalidad.
const (
	FeatureReservations Feature = "reservations"
	FeatureParking      Feature = "parking"
	FeatureSharedAreas  Feature = "shared_areas"
	FeatureGuestChat    Feature = "guest_chat"
	FeaturePayroll      Feature = "payroll"
	FeatureSmartLocks   Feature = "smart_locks"
)

// Features es el catalogo de banderas, en el orden en que se publican.
var Features = []Feature{
	FeatureReservations, FeatureParking, FeatureSharedAreas,
	FeatureGuestChat, FeaturePayroll, FeatureSmartLocks,
}
