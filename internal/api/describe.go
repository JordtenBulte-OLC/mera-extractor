// internal/api/describe.go
package api

import (
	"encoding/json"
	"net/http"

	"mera-extractor/internal/mxcli"
)

type describeRequest struct {
	MprPath       string `json:"mprPath"`
	UnitType      string `json:"unitType"`
	QualifiedName string `json:"qualifiedName"`
}

func (s *Server) handleDescribe(w http.ResponseWriter, r *http.Request) {
	var req describeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, r, http.StatusBadRequest, err)
		return
	}

	resp := map[string]string{"qualifiedName": req.QualifiedName}
	mdl, err := mxcli.Describe(r.Context(), mxcli.DescribeRequest{
		MprPath: req.MprPath, UnitType: req.UnitType, QualifiedName: req.QualifiedName,
	})
	if err != nil {
		resp["warning"] = err.Error() // degrade, don't fail — manual §1.4
	} else {
		resp["mdl"] = mdl
	}
	writeJSON(w, http.StatusOK, resp)
}