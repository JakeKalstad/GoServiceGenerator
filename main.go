package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"html/template"
	"io/ioutil"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fgrid/uuid"
)

const emptyUUID = "00000000-0000-0000-0000-000000000000"

func isUpper(c byte) bool {
	return c >= 'A' && c <= 'Z'
}
func toLower(c byte) byte {
	return c + 32
}

func Underscore(s string) string {
	r := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isUpper(c) {
			if i > 0 && i+1 < len(s) && (!isUpper(s[i-1]) || !isUpper(s[i+1])) {
				r = append(r, '_', toLower(c))
			} else {
				r = append(r, toLower(c))
			}
		} else {
			r = append(r, c)
		}
	}
	return string(r)
}

func (a *AppConfig) Underscore(s string) string {
	return Underscore(s)
}

type Column struct {
	Name string
	Type string
	Null bool
}

type DataConfig struct {
	Name    string
	Columns []Column
	Routing Routing
}

type Routing map[string]string

type AppConfig struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	Email     string `json:"email"`
	MsTimeout int    `json:"ms_timeout"`
	Data      []DataConfig
}

func (a *AppConfig) load(path string) {
	if len(path) <= 0 {
		panic("No configuration file defined!")
	}
	content, err := ioutil.ReadFile(path)
	if err != nil {
		panic(err)
	}
	err = json.Unmarshal(content, a)
	if err != nil {
		panic(err)
	}
}

type routeTempData struct {
	Name  string
	Lower string
}

func (a *AppConfig) GetRoutes() string {
	funcTemp := `func (app *application) {{.Lower}}(w http.ResponseWriter, r *http.Request) {
	app.runner(w, r, func() *chanRet {
		return execByVerb(r,
			func(body []byte) ([]byte, error) {
				data := &db.{{.Name}}{}
				err := json.Unmarshal(body, &data)
        if err != nil {
          app.Publish("error", &AppError{
            error:  err,
            Action: "post request - {{.Name}}",
            Data: string(body),
          })
        }
        status := "created"
        if len(data.UUID) != 0 {
          status = "updated"
        }
				err = app.db.Insert{{.Name}}(data)
			  if err == nil {
          app.Publish("{{.Lower}}." + status, &EventMessage{
            Status: status,
            DataType: "{{.Name}}",
            Data: data,
          })
        } else {
          app.Publish("error", &AppError{
                error:  err,
                Action: "post request - {{.Name}}",
                Data:   string(body),
          })
          return nil, err
        }
        return json.Marshal(data)
			},
			func(key string) ([]byte, error) {
				result, err := app.db.Get{{.Name}}(key)
				if err != nil {
          app.Publish("error", &AppError{
                error:  err,
                Action: "get request - {{.Name}}",
                Data:   key,
          })
					return nil, err
				}
				return json.Marshal(result)
			},
			func(key string) error {
			  err := app.db.Delete{{.Name}}(key)
			  if err == nil {
          app.Publish("{{.Lower}}.deleted", &EventMessage{
            Status: "deleted",
            DataType: "{{.Name}}",
            Data: &response{Message: key},
          })
        } else {
          app.Publish("error", &AppError{
                error:  err,
                Action: "delete request - {{.Name}}",
                Data:   key,
          })
        }
        return err
			})
	})
}
`
	specialRoute := `func (app *application) %s(w http.ResponseWriter, r *http.Request) {
	app.runner(w, r, func() *chanRet {
		return execByVerb(r,
			func(body []byte) ([]byte, error) {
				return nil, nil
			},
			func(key string) ([]byte, error) {
				result, err := app.db.Get%s(key)
				if err != nil {
					return nil, err
				}
				return json.Marshal(result)
			},
			func(key string) error {
        return nil
			})
	})
}
`
	tmpl, err := template.New("func").Parse(funcTemp)
	if err != nil {
		panic(err)
	}
	routes := ""
	buf := bytes.NewBuffer([]byte{})
	for _, data := range a.Data {
		rData := &routeTempData{
			Name:  data.Name,
			Lower: strings.ToLower(data.Name),
		}
		buf.Reset()
		tmpl.Execute(buf, rData)
		routes += buf.String()
		for _, route := range data.Routing {
			routes += fmt.Sprintf(specialRoute, strings.ToLower(data.Name)+"By"+route, data.Name+"By"+route)
		}
	}
	return routes
}

func (a *AppConfig) MuxHandlers() string {
	muxTemp := `mux.Handle("/%s", contextualize(time.Millisecond*` + strconv.Itoa(a.MsTimeout) + `)(http.HandlerFunc(app.%s)))
  `
	muxes := ""
	for _, data := range a.Data {
		muxes += fmt.Sprintf(muxTemp, Underscore(data.Name), strings.ToLower(data.Name))
		for key, route := range data.Routing {
			muxes += fmt.Sprintf(muxTemp, Underscore(data.Name)+"/"+key, strings.ToLower(data.Name)+"By"+route)
		}
	}
	return muxes
}

func (data *DataConfig) UniqueID() string {
	return "strconv.Itoa(time.Now().Nanosecond())"
}
func (data *DataConfig) GetCreate() string {
	query := ` 
            CREATE TABLE IF NOT EXISTS %s (
                uuid            UUID PRIMARY KEY,
                %s
                created     TIMESTAMP WITH TIME ZONE NULL,
                updated     TIMESTAMP WITH TIME ZONE NULL,
                deleted    TIMESTAMP WITH TIME ZONE NULL
            );`
	columnDef := ""
	for _, c := range data.Columns {
		nullable := " NOT NULL"
		if c.Null {
			nullable = " NULL"
		}
		columnDef += Underscore(c.Name) + " " + getPsqlStringByType(c.Type) + nullable + ", "
	}
	return fmt.Sprintf(query, Underscore(data.Name), columnDef)
}

func (data *DataConfig) GetName() string {
	return Underscore(data.Name)
}
func (data *DataConfig) GetSelectParams() string {
	cont := "uuid"
	for _, c := range data.Columns {
		cont += "," + Underscore(c.Name)
	}
	return cont
}
func (data *DataConfig) GetInsert() string {
	query := `INSERT INTO %s ("uuid"%s
  VALUES(%s, now()) 
  ON CONFLICT("uuid") DO UPDATE SET 
  %s`
	columnDef := ""
	valueDef := "$1"
	setDef := "updated=now()"
	valCnt := 1
	for _, c := range data.Columns {
		valCnt++
		setDef += ", " + Underscore(c.Name) + "=$" + strconv.Itoa(valCnt)
		columnDef += "," + Underscore(c.Name)
		valueDef += ",$" + strconv.Itoa(valCnt)
	}
	columnDef += ", created)"
	return fmt.Sprintf(query, Underscore(data.Name), columnDef, valueDef, setDef)
}

// Name	Aliases	Description
// cidr	 	IPv4 or IPv6 network address
// double precision	float8	double precision floating-point number (8 bytes)
// inet	 	IPv4 or IPv6 host address
// macaddr	 	MAC (Media Access Control) address
// money	 	currency amount
// numeric [ (p, s) ]	decimal [ (p, s) ]	exact numeric of selectable precision
type dataType struct {
	GoString   string
	PsqlString string
	TEST       float32
}

var dataTypes = map[string]dataType{
	"TEXT": dataType{
		GoString:   "string",
		PsqlString: "TEXT",
	},
	"UUID": dataType{
		GoString:   "string",
		PsqlString: "uuid",
	},
	"INTEGER": dataType{
		GoString:   "int",
		PsqlString: "INTEGER",
	},
	"SMALL": dataType{
		GoString:   "int16",
		PsqlString: "smallint",
	},
	"BIG": dataType{
		GoString:   "int64",
		PsqlString: "bigint",
	},
	"FLOAT": dataType{
		GoString:   "float32",
		PsqlString: "real",
	},
	"DOUBLE": dataType{
		GoString:   "float64",
		PsqlString: "double",
	},
	"POINT": dataType{
		GoString:   "string",
		PsqlString: "point",
	},
	"BOOL": dataType{
		GoString:   "bool",
		PsqlString: "BOOLEAN",
	},
	"TIME": dataType{
		GoString:   "int64",
		PsqlString: "TIMESTAMP WITH TIME ZONE",
	},
}

func getGoStringByType(t string) string {
	return dataTypes[t].GoString
}

func getPsqlStringByType(t string) string {
	return dataTypes[t].PsqlString
}

func (data *DataConfig) GetGoStruct() string {
	structDef := `type %s struct {
    UUID  string ` + "`" + `json:"uuid"` + "`" + `%s
}`
	dataDef := ""
	for _, c := range data.Columns {
		entry := c.Name + " " + getGoStringByType(c.Type) + " `" + `json:"` + Underscore(c.Name) + "\"`"
		dataDef += fmt.Sprintf(`
    %s`, entry)
	}
	return fmt.Sprintf(structDef, data.Name, dataDef)
}

func (data *DataConfig) CleanData() string {
	actions := ""
	for _, c := range data.Columns {
		if strings.ToUpper(c.Type) == "UUID" {
			actions += fmt.Sprintf(`if len(data.%s) == 0 {
    data.%s = "00000000-0000-0000-0000-000000000000"
  }
        `, c.Name, c.Name)
		}
	}
	return actions
}

func (data *DataConfig) GetGoParams() string {
	params := ""
	for _, c := range data.Columns {
		params += ", data." + c.Name
	}
	return params
}
func (data *DataConfig) GetGoRefParams() string {
	params := ""
	for _, c := range data.Columns {
		params += ", &data." + c.Name
	}
	return params
}

type source struct {
	UUID string
	Name string
	Body *bytes.Buffer
}

func newSource(name string, pkg string, body *bytes.Buffer) source {
	newUUID := uuid.NewV5(uuid.NewNamespaceUUID(pkg), []byte(name)).String()
	return source{
		UUID: newUUID,
		Name: name,
		Body: body,
	}
}

type sourceMap struct {
	sourceData map[string][]source
}

func createMain(config *AppConfig) source {
	buf := bytes.NewBuffer([]byte{})
	if err := template.Must(template.ParseFiles("templates/app.tmpl")).Execute(buf, config); err != nil {
		panic(err)
	}
	return newSource("main", "main", buf)
}

func createDb(config *AppConfig) source {
	buf := bytes.NewBuffer([]byte{})
	if err := template.Must(template.ParseFiles("templates/data.tmpl")).Execute(buf, config); err != nil {
		panic(err)
	}
	return newSource("sql", "data", buf)
}

func generateFromDisk() bool {
	appConfigPath := flag.String("conf", "", "where is the configuration stored?")
	flag.Parse()
	if len(*appConfigPath) == 0 {
		return false
	}
	config := &AppConfig{}
	config.load(*appConfigPath)
	sMap := sourceMap{
		sourceData: map[string][]source{},
	}
	sMap.sourceData["main"] = append(sMap.sourceData["main"], createMain(config))
	sMap.sourceData["data"] = append(sMap.sourceData["data"], createDb(config))
	err := os.Chdir("gen_src")
	if err != nil {
		panic(err)
	}

	err = os.RemoveAll("./data")
	if err != nil {
		panic(err)
	}
	for k, w := range sMap.sourceData {
		if k != "main" {
			err = os.Mkdir(k, 0755)
			if err != nil {
				panic(err)
			}
			err = os.Chdir("./" + k)
			if err != nil {
				panic(err)
			}
		}
		for _, sauce := range w {
			err = ioutil.WriteFile(sauce.Name+".go", bytes.NewBufferString(html.UnescapeString(sauce.Body.String())).Bytes(), 0755)
			if err != nil {
				panic(err)
			}
		}

		if k != "main" {
			err = os.Chdir("./..")
			if err != nil {
				panic(err)
			}
		}
	}
	return true
}
func main() {
	if generateFromDisk() {
		return
	}
	mux := http.NewServeMux()
	mux.Handle("/generate.tar.gz", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		config := &AppConfig{}
		err := json.Unmarshal(bytes.NewBufferString(r.URL.Query().Get("config")).Bytes(), config)
		if err != nil {
			fmt.Printf("%+v", err)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte{})
			return
		}
		sMap := sourceMap{
			sourceData: map[string][]source{},
		}
		sMap.sourceData["main"] = append(sMap.sourceData["main"], createMain(config))
		sMap.sourceData["data"] = append(sMap.sourceData["data"], createDb(config))
		dirName := config.Name
		if err != nil {
			fmt.Printf("%+v", err)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte{})
			return
		}
		gzw := gzip.NewWriter(w)
		defer gzw.Close()
		tw := tar.NewWriter(gzw)
		if err := tw.Flush(); err != nil {
			fmt.Printf("%+v", err)
		}
		defer tw.Close()
		for k, sa := range sMap.sourceData {
			subdirName := dirName
			if k != "main" {
				subdirName = dirName + "/" + k
			}
			if err != nil {
				fmt.Printf("%+v", err)
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte{})
				return
			}
			for _, sauce := range sa {
				file := subdirName + "/" + sauce.Name + ".go"
				contentBytes := bytes.NewBufferString(html.UnescapeString(sauce.Body.String())).Bytes()
				header := &tar.Header{
					Mode:     0777,
					Typeflag: tar.TypeReg,
					Name:     strings.TrimPrefix(file, string(filepath.Separator)),
					ModTime:  time.Now(),
					Size:     int64(len(contentBytes)),
				}
				if err := tw.WriteHeader(header); err != nil {
					fmt.Printf("%+v", err)
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte{})
					return
				}
				b, err := tw.Write(contentBytes)
				if err != nil {
					fmt.Printf("%+v", err)
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte{})
					return
				}
				fmt.Println(b)
			}
		}
	}))
	server := &http.Server{
		Addr: fmt.Sprintf(
			"%s:%d",
			"0.0.0.0",
			9111,
		),
		Handler: mux,
	}
	serverStartup := make(chan error, 1)

	go func() {
		serverStartup <- server.ListenAndServe()
	}()
	osSignals := make(chan os.Signal, 1)
	signal.Notify(
		osSignals,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGKILL,
		syscall.SIGQUIT,
	)
	select {
	case sig := <-osSignals:
		fmt.Printf(sig.String())
	case err := <-serverStartup:
		fmt.Printf(err.Error())
	}
	fmt.Printf("\n\nADIOS! TOT ZIENS! HASTA LUEGO!\n")
}
