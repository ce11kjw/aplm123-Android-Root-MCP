package ops

// publicActionAliases maps the stable schema vocabulary to the older internal
// operation names. Keeping this translation at the dispatch boundary lets the
// implementation retain backwards-compatible aliases without leaking them
// into the public contract.
var publicActionAliases = map[string]map[string]string{
	"aplm_app_list": {
		"list": "all",
	},
	"aplm_app_info": {
		"get": "info",
	},
	"aplm_app_manage": {
		"launch": "start",
	},
	"aplm_root_info": {
		"detect":       "get",
		"capabilities": "get",
		"self_test":    "check",
	},
	"aplm_root_module": {
		"remove": "delete",
		"logs":   "log",
	},
	"aplm_systemless": {
		"apply": "stage",
	},
	"aplm_property": {
		"reset": "delete",
	},
	"aplm_audio": {
		"volume": "set_volume",
	},
	"aplm_connectivity": {
		"get":           "status",
		"airplane_mode": "airplane",
	},
	"aplm_input_method": {
		"get": "current",
	},
	"aplm_accessibility": {
		"list": "get",
	},
	"aplm_network": {
		"ports":        "port_check",
		"wifi":         "wifi_info",
		"connectivity": "internet_test",
	},
	"aplm_log": {
		"mcp": "service",
	},
	"aplm_diagnostics": {
		"self_test": "check",
		"collect":   "report",
	},
	"aplm_backup": {
		"remove": "delete",
	},
}

func normalizePublicRequest(req Request) Request {
	rawAction, ok := req.Args["action"].(string)
	if !ok {
		return req
	}
	action := normalizeTool(rawAction)
	aliases := publicActionAliases[req.Tool]
	target, exists := aliases[action]
	if !exists {
		return req
	}
	cloned := make(map[string]any, len(req.Args))
	for key, value := range req.Args {
		cloned[key] = value
	}
	cloned["action"] = target
	req.Args = cloned
	return req
}
