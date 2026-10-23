package health

// Status es el estado del servicio.
type Status struct {
	Status      string `json:"status" enum:"ok,degraded"`
	Environment string `json:"environment"`
	Database    string `json:"database" enum:"up,down"`
}
