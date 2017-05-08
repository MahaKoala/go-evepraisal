package main

//go:generate go-bindata -prefix resources/ resources/...

import (
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"strings"

	"github.com/elazarl/go-bindata-assetfs"
	"github.com/evepraisal/go-evepraisal/parsers"
	"github.com/husobee/vestigo"
)

var serverPort = 8080
var templates = MustLoadTemplateFiles()

func MustLoadTemplateFiles() *template.Template {
	t := template.New("root")
	for _, path := range AssetNames() {
		if strings.HasPrefix(path, "templates/") {
			tmpl := t.New(strings.TrimPrefix(path, "templates/"))
			fileContents, err := Asset(path)
			if err != nil {
				panic(err)
			}

			_, err = tmpl.Parse(string(fileContents))
			if err != nil {
				panic(err)
			}
		}
	}
	return t
}

type AppraisalPage struct {
	r parsers.ParserResult
}

func IndexHandler(w http.ResponseWriter, r *http.Request) {
	err := templates.ExecuteTemplate(w, "main.html", struct{}{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func AppraiseHandler(w http.ResponseWriter, r *http.Request) {
	body := r.FormValue("body")
	log.Printf("%#v", body)
	result, _ := parsers.AllParser(parsers.StringToInput(body))
	log.Printf("%#v", result)
	json.NewEncoder(w).Encode(result)
}

func main() {
	log.Println("Included assets:")
	for _, filename := range AssetNames() {
		log.Println(" - ", filename)
	}

	router := vestigo.NewRouter()
	router.Get("/", IndexHandler)
	router.Post("/appraise", AppraiseHandler)

	mux := http.NewServeMux()

	// Route our bundled static files
	var fs = &assetfs.AssetFS{Asset: Asset, AssetDir: AssetDir, AssetInfo: AssetInfo, Prefix: "/static/"}
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(fs)))

	// Mount our web app router to root
	mux.Handle("/", router)
	http.ListenAndServe(":8080", mux)

	log.Printf("Starting http server on port %d", serverPort)
	log.Fatal(http.ListenAndServe(":8080", router))
}
