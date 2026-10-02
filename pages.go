package main

import (
	"bytes"
	"html/template"
	"log"
	"net/http"
)

type pageData struct {
	LoggedIn      bool
	CharacterName string
	SSOConfigured bool
}

func (app *application) render(w http.ResponseWriter, status int, page string, data pageData) {
	ts, err := template.New("base").ParseFS(templatesFS, "templates/base.html", "templates/"+page)
	if err != nil {
		log.Printf("parse template %s: %v", page, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	buf := new(bytes.Buffer)
	if err := ts.ExecuteTemplate(buf, "base", data); err != nil {
		log.Printf("execute template %s: %v", page, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

func (app *application) handleHome(w http.ResponseWriter, r *http.Request) {
	data := pageData{
		LoggedIn:      app.sessions.GetBool(r.Context(), "authenticated"),
		CharacterName: app.sessions.GetString(r.Context(), "character_name"),
		SSOConfigured: app.cfg.eveClientID != "",
	}
	app.render(w, http.StatusOK, "home.html", data)
}

func (app *application) handleAdmin(w http.ResponseWriter, r *http.Request) {
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(r.Context(), "character_name"),
		SSOConfigured: app.cfg.eveClientID != "",
	}
	app.render(w, http.StatusOK, "admin.html", data)
}
