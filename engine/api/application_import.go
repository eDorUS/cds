package main

import (
	"fmt"
	"io/ioutil"
	"net/http"

	"gopkg.in/yaml.v2"

	"github.com/go-gorp/gorp"
	"github.com/gorilla/mux"
	"github.com/hashicorp/hcl"

	"github.com/ovh/cds/engine/api/application"
	"github.com/ovh/cds/engine/api/context"
	"github.com/ovh/cds/engine/api/environment"
	"github.com/ovh/cds/engine/api/group"
	"github.com/ovh/cds/engine/api/hook"
	"github.com/ovh/cds/engine/api/notification"
	"github.com/ovh/cds/engine/api/pipeline"
	"github.com/ovh/cds/engine/api/poller"
	"github.com/ovh/cds/engine/api/project"
	"github.com/ovh/cds/engine/api/sanity"
	"github.com/ovh/cds/engine/log"
	"github.com/ovh/cds/sdk"
	"github.com/ovh/cds/sdk/exportentities"
)

func importApplicationHandler(w http.ResponseWriter, r *http.Request, db *gorp.DbMap, c *context.Ctx) error {
	vars := mux.Vars(r)
	key := vars["permProjectKey"]
	format := r.FormValue("format")
	forceUpdate := FormBool(r, "forceUpdate")

	// Load project
	proj, errp := project.Load(db, key, c.User, project.LoadOptions.Default, project.LoadOptions.WithEnvironments)
	if errp != nil {
		return sdk.WrapError(errp, "importApplicationHandler> Unable to load project %s", key)
	}

	if err := group.LoadGroupByProject(db, proj); err != nil {
		return sdk.WrapError(errp, "importApplicationHandler> Unable to load project permissions %s", key)
	}

	// Get body
	data, errRead := ioutil.ReadAll(r.Body)
	if errRead != nil {
		return sdk.WrapError(sdk.ErrWrongRequest, "importApplicationHandler> Unable to read body")
	}

	// Compute format
	f, errF := exportentities.GetFormat(format)
	if errF != nil {
		return sdk.WrapError(sdk.ErrWrongRequest, "importApplicationHandler> Unable to get format : %s", errF)
	}

	// Parse the pipeline
	payload := &exportentities.Application{}
	var errorParse error
	switch f {
	case exportentities.FormatJSON, exportentities.FormatHCL:
		errorParse = hcl.Unmarshal(data, payload)
	case exportentities.FormatYAML:
		errorParse = yaml.Unmarshal(data, payload)
	}

	if errorParse != nil {
		log.Warning("importApplicationHandler> Cannot parsing: %s\n", errorParse)
		return sdk.ErrWrongRequest
	}

	// Check if application exists
	exist, errE := application.Exists(db, proj.ID, payload.Name)
	if errE != nil {
		return sdk.WrapError(errE, "importApplicationHandler> Unable to check if application %s exists", payload.Name)
	}

	//Transform payload to a sdk.Application
	app, errP := payload.Application()
	if errP != nil {
		return sdk.WrapError(errP, "importApplicationHandler> Unable to parse application %s", payload.Name)
	}

	// Load group in permission
	for i := range app.ApplicationGroups {
		eg := &app.ApplicationGroups[i]
		g, errg := group.LoadGroup(db, eg.Group.Name)
		if errg != nil {
			return sdk.WrapError(errg, "importApplicationHandler> Error loading groups for permission")
		}
		eg.Group = *g
	}

	allMsg := []sdk.Message{}
	msgChan := make(chan sdk.Message, 1)
	done := make(chan bool)

	go func() {
		for {
			msg, ok := <-msgChan
			allMsg = append(allMsg, msg)
			if !ok {
				done <- true
				return
			}
		}
	}()

	tx, errBegin := db.Begin()
	if errBegin != nil {
		return sdk.WrapError(errBegin, "importApplicationHandler> Cannot start transaction")
	}

	defer tx.Rollback()

	var globalError error

	if exist && !forceUpdate {
		return sdk.ErrApplicationExist
	}

	//Check that all pipelines exists
	for _, p := range app.Pipelines {
		ok, err := pipeline.ExistPipeline(tx, proj.ID, p.Pipeline.Name)
		if err != nil {
			return sdk.WrapError(errBegin, "importApplicationHandler> Unable to check pipeline %s", p.Pipeline.Name)
		}
		if !ok {
			msgChan <- sdk.NewMessage(sdk.MsgAppImportPipelineNotFound, p.Pipeline.Name)
			globalError = sdk.ErrPipelineNotFound
		}

		//Checks dest application exists
		for _, t := range p.Triggers {
			if t.DestApplication.Name != app.Name {
				ok, err := application.Exists(tx, proj.ID, t.DestApplication.Name)
				if err != nil {
					return sdk.WrapError(errBegin, "importApplicationHandler> Unable to check application %s", t.DestApplication.Name)
				}
				if !ok {
					msgChan <- sdk.NewMessage(sdk.MsgAppImportAppNotFound, t.DestApplication.Name)
					globalError = sdk.ErrApplicationNotFound
				}
			}
			//Check src env exists
			if t.SrcEnvironment.Name != sdk.DefaultEnv.Name {
				ok, err := environment.Exists(tx, proj.Key, t.SrcEnvironment.Name)
				if err != nil {
					return sdk.WrapError(errBegin, "importApplicationHandler> Unable to check env %s", t.SrcEnvironment.Name)
				}
				if !ok {
					msgChan <- sdk.NewMessage(sdk.MsgAppImportEnvNotFound, t.SrcEnvironment.Name)
					globalError = sdk.ErrNoEnvironment
				}
			}
			//Check dest env exists
			if t.DestEnvironment.Name != sdk.DefaultEnv.Name {
				ok, err := environment.Exists(tx, proj.Key, t.DestEnvironment.Name)
				if err != nil {
					return sdk.WrapError(errBegin, "importApplicationHandler> Unable to check env %s", t.DestEnvironment.Name)
				}
				if !ok {
					msgChan <- sdk.NewMessage(sdk.MsgAppImportEnvNotFound, t.DestEnvironment.Name)
					globalError = sdk.ErrNoEnvironment
				}
			}
		}
	}

	if globalError == nil {
		if exist {
			//globalError = application.ImportUpdate(tx, proj, app, msgChan, c.User)
		} else {
			globalError = application.Import(tx, proj, app, app.RepositoriesManager, c.User, msgChan)
		}

		if globalError == nil && app.RepositoriesManager != nil {
			//Manage hook
			for _, h := range app.Hooks {
				for _, p := range app.Pipelines {
					if p.Pipeline.Name == h.Pipeline.Name {
						h.Pipeline = p.Pipeline
						break
					}
				}
				log.Debug("importApplicationHandler> Insert hook %s(%d)", h.Pipeline.Name, h.Pipeline.ID)
				if h.Pipeline.ID == 0 {
					msgChan <- sdk.NewMessage(sdk.MsgAppImportPipelineNotFound, h.Pipeline.Name)
					return sdk.WrapError(sdk.ErrPipelineNotFound, "importApplicationHandler> Pipeline not found for hook %s")
				}
				if _, err := hook.CreateHook(db, proj.Key, app.RepositoriesManager, app.RepositoryFullname, app, &h.Pipeline); err != nil {
					return sdk.WrapError(err, "importApplicationHandler> Unable to insert hook on application %s/%s on pipeline %s", proj.Key, app.Name, h.Pipeline.Name)
				}
				if msgChan != nil {
					msgChan <- sdk.NewMessage(sdk.MsgHookCreated, app.RepositoryFullname, &h.Pipeline.Name)
				}
			}

			//Manager Pollers
			for _, h := range app.RepositoryPollers {
				for _, p := range app.Pipelines {
					if p.Pipeline.Name == h.Pipeline.Name {
						h.Pipeline = p.Pipeline
						break
					}
				}
				log.Debug("importApplicationHandler> Insert poller %s(%d)", h.Pipeline.Name, h.Pipeline.ID)
				if h.Pipeline.ID == 0 {
					msgChan <- sdk.NewMessage(sdk.MsgAppImportPipelineNotFound, h.Pipeline.Name)
					return sdk.WrapError(sdk.ErrPipelineNotFound, "importApplicationHandler> Pipeline %s not found", h.Pipeline.Name)
				}

				poll := &sdk.RepositoryPoller{
					Application: *app,
					Pipeline:    h.Pipeline,
				}

				if err := poller.Insert(db, poll); err != nil {
					return sdk.WrapError(err, "importApplicationHandler> Unable to insert poller on application %s/%s on pipeline %s", proj.Key, app.Name, h.Pipeline.Name)
				}
				if msgChan != nil {
					msgChan <- sdk.NewMessage(sdk.MsgPollerCreated, app.RepositoryFullname, &h.Pipeline.Name)
				}
			}
		}

		if globalError == nil {
			for _, notif := range app.Notifications {
				fmt.Printf("%+v\n", notif)
				var pipID, envID int64
				for _, p := range app.Pipelines {
					if p.Pipeline.Name == notif.Pipeline.Name {
						pipID = p.Pipeline.ID
						break
					}
				}

				if notif.Environment.Name == "" || notif.Environment.Name == sdk.DefaultEnv.Name {
					notif.Environment = sdk.DefaultEnv
					envID = sdk.DefaultEnv.ID
				} else {
					for _, e := range proj.Environments {
						if e.Name == notif.Environment.Name {
							envID = e.ID
							break
						}
					}
				}

				if pipID == 0 {
					return sdk.WrapError(sdk.ErrPipelineNotFound, "importApplicationHandler> Pipeline %s not found for notification %+v", notif.Pipeline.Name, notif)
				}

				if envID == 0 {
					return sdk.WrapError(sdk.ErrNoEnvironment, "importApplicationHandler> Environment %s not found for notification %+v", notif.Pipeline.Name, notif)
				}

				if err := notification.InsertOrUpdateUserNotificationSettings(tx, app.ID, pipID, envID, &notif); err != nil {
					return sdk.WrapError(err, "importApplicationHandler> Unable to insert notification on application %s/%s on pipeline %s", proj.Key, app.Name, notif.Pipeline.Name)
				}
			}
		}
		/*
			for _, sched := range app.Schedulers {

			}
		*/
	}
	close(msgChan)
	<-done

	al := r.Header.Get("Accept-Language")
	msgListString := []string{}

	for _, m := range allMsg {
		s := m.String(al)
		if s != "" {
			var msgFound bool
			for _, os := range msgListString {
				if os == s {
					msgFound = true
				}
			}
			if !msgFound {
				msgListString = append(msgListString, s)
			}
		}
	}

	log.Debug("importApplicationHandler >>> %v", msgListString)

	if globalError != nil {
		myError, ok := globalError.(*sdk.Error)
		if ok {
			return WriteJSON(w, r, msgListString, myError.Status)
		}
		return sdk.WrapError(globalError, "importApplicationHandler> Unable import pipeline")
	}

	if err := project.UpdateLastModified(tx, c.User, proj); err != nil {
		return sdk.WrapError(err, "importApplicationHandler> Unable to update project")
	}

	if err := tx.Commit(); err != nil {
		return sdk.WrapError(err, "importPipelineHandler> Cannot commit transaction")
	}

	var errapp error
	proj.Applications, errapp = application.LoadAll(db, proj.Key, c.User, application.LoadOptions.Default)
	if errapp != nil {
		return sdk.WrapError(errapp, "importPipelineHandler> Unable to reload applications for project %s", proj.Key)
	}

	if err := sanity.CheckApplication(db, proj, app); err != nil {
		return sdk.WrapError(err, "importPipelineHandler> Cannot check warnings")
	}

	return WriteJSON(w, r, msgListString, http.StatusOK)
}
