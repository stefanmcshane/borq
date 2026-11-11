package webui

import (
	"net/http"
	"net/url"

	"github.com/stefanmcshane/borq/api/appapi"
)

type spacePocketIdPageData struct {
	// Services lists hosted Pocket ID instances in the selected space.
	Services []appapi.PocketID
	// Selected contains the instance explicitly opened for editing.
	Selected appapi.PocketID
	// Operation keeps accepted changes visible, including deletion.
	Operation appapi.Operation
}

func (web handler) loadSpacePocketIdPage(request *http.Request, selection url.Values, page pageData) (pageData, error) {
	base := spacePath(page.Organization, page.Space.Name) + "/managed-addons/pocketid"
	var err error
	page.SpacePocketId.Services, err = requestApi[[]appapi.PocketID](request.Context(), web.api, http.MethodGet, base, nil)
	if err != nil {
		return page, err
	}
	operation := selection.Get("pocketIdOperation")
	if name := selection.Get("spacePocketId"); name != "" {
		page.SpacePocketId.Selected, err = requestApi[appapi.PocketID](request.Context(), web.api, http.MethodGet, base+"/"+url.PathEscape(name), nil)
		if err != nil {
			return page, err
		}
		operation = page.SpacePocketId.Selected.Operation
	}
	if operation != "" {
		page.SpacePocketId.Operation, err = requestApi[appapi.Operation](request.Context(), web.api, http.MethodGet, "/api/v1/pocketid-operations/"+url.PathEscape(operation), nil)
		if err != nil {
			return page, err
		}
		if page.SpacePocketId.Operation.SpaceID != page.Space.ID {
			return page, apiProblem{status: http.StatusNotFound, message: "The operation belongs to another space."}
		}
	}
	return page, nil
}

func (web handler) executeSpacePocketIdAction(request *http.Request, result actionResult) (actionResult, error) {
	form := request.PostForm
	name := form.Get("name")
	base := spacePath(form.Get("organization"), form.Get("space")) + "/managed-addons/pocketid/" + url.PathEscape(name)
	result.selection.Set("view", "space-pocketid")
	result.selection.Set("spacePocketId", name)
	if form.Get("action") == "enroll-space-pocketid" {
		enrollment, err := requestApi[appapi.PocketIDEnrollment](request.Context(), web.api, http.MethodPost, base+"/enrollment", nil)
		if err != nil {
			return result, err
		}
		result.pocketIdEnrollment = enrollment.URL
		result.notice = "One-time administrator enrollment link created. Open it privately to register a passkey."
		return result, nil
	}
	method := http.MethodDelete
	var body any
	if form.Get("action") == "put-space-pocketid" {
		method = http.MethodPut
		// Generated pointers preserve omission so blank advanced fields use server defaults.
		body = appapi.PocketIDInput{
			IssuerURL: pocketIdFormValue(form, "issuerUrl"), AdminUsername: pocketIdFormValue(form, "adminUsername"), AdminEmail: pocketIdFormValue(form, "adminEmail"),
			Storage: pocketIdFormValue(form, "storage"), StorageClass: pocketIdFormValue(form, "storageClass"), Image: pocketIdFormValue(form, "image"), CertificateIssuer: pocketIdFormValue(form, "certificateIssuer"),
		}
		if form.Get("apply") == "true" {
			base += "?apply=true"
		}
	}
	service, err := requestApi[appapi.PocketID](request.Context(), web.api, method, base, body)
	if err != nil {
		return result, err
	}
	result.notice = "Pocket ID operation accepted. Updates reuse its SQLite data and encryption key, with a brief interruption."
	if method == http.MethodDelete {
		result.selection.Del("spacePocketId")
		if service.Operation != "" {
			result.selection.Set("pocketIdOperation", service.Operation)
		}
		result.notice = "Pocket ID removal accepted. Its SQLite volume and encryption key will be retained."
	}
	return result, nil
}

func pocketIdFormValue(form url.Values, name string) *string {
	value := form.Get(name)
	if value == "" {
		return nil
	}
	return &value
}
