package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"

	_ "github.com/go-sql-driver/mysql"
	"github.com/mileusna/crontab"
)

var (
	workdir, templatesFileDir, freepbxConf string
	serveraddr, cron                       string
	db                                     *sql.DB
	listenport                             int
)

//CiscoIPPhoneDirectory struct
type CiscoIPPhoneDirectory struct {
	List []ADuser
}

//ADuser struct
type ADuser struct {
	GroupID                    int
	DisplayName, MAC           string
	PhoneNumber, Company       string
	FirstName, LastName, Title string
	Department, GroupName      string
	WorkPhoneNumber            string
	Email, Description, Home   string
}

//CiscoIPPhoneMenu struct
type CiscoIPPhoneMenu struct {
	List []MenuItem
}

//MenuItem struct
type MenuItem struct {
	Name, URL string
}

//Group struct
type Group struct {
	ID                     int
	GroupName, Description string
	Users                  string
}

//Grandsteam struct
type Grandsteam struct {
	GroupList []Group
	UserList  []ADuser
}

//Create phonebook for cisco phones
func ciscoPhonebook(templates *template.Template, groups []Group, Users []ADuser) error {
	var (
		menuitems []MenuItem
		menuitem  MenuItem
	)

	for _, group := range groups {
		var usersInGroup []ADuser
		filename := fmt.Sprintf("%s.%s", group.GroupName, "xml")

		menuitem.Name = group.Description
		menuitem.URL = fmt.Sprintf("http://%s:%d/%s", serveraddr, listenport, filename)
		menuitems = append(menuitems, menuitem)
		for _, User := range Users {
			if User.GroupID == group.ID {
				usersInGroup = append(usersInGroup, User)
			}
		}
		directoryentryfile := filepath.Join(workdir, filename)

		if group.Users != "[]" {
			f, err := os.Create(directoryentryfile)
			if err != nil {
				return err
			}
			templates.ExecuteTemplate(f, "cisco-ipphonedirectory.xml.tpl", CiscoIPPhoneDirectory{usersInGroup})
			f.Close()
		}
	}

	f, err := os.Create(filepath.Join(workdir, "directory.xml"))
	if err != nil {
		return err
	}
	templates.ExecuteTemplate(f, "cisco-ipphonemenu.xml.tpl", CiscoIPPhoneMenu{menuitems})
	f.Close()

	return nil
}

//Create phonebook for grandstream phones
func grandstreamPhonebook(templates *template.Template, groups []Group, Users []ADuser) error {
	f, err := os.Create(filepath.Join(workdir, "phonebook.xml"))
	if err != nil {
		return err
	}
	templates.ExecuteTemplate(f, "grandstream-phonebook.xml.tpl", Grandsteam{groups, Users})
	f.Close()

	return nil
}

//Create phonebook for website phones
func websitePhonebook(templates *template.Template, groups []Group, Users []ADuser) error {
	f, err := os.Create(filepath.Join(workdir, "website-phonebook.xml"))
	if err != nil {
		return err
	}
	templates.ExecuteTemplate(f, "website-phonebook.xml.tpl", Grandsteam{groups, Users})
	f.Close()

	return nil
}

//Create ADuser slice
func selectUsers(usersarr string, groupID int, groupName string) ([]ADuser, error) {
	var (
		err        error
		User       ADuser
		users      []string
		sliceUsers []ADuser
	)

	err = json.Unmarshal([]byte(usersarr), &users)
	if err != nil {
		return sliceUsers, err
	}

	if len(users) > 0 {
		usersid := make([]interface{}, len(users))
		for i, id := range users {
			usersid[i] = id
		}

		query := `
	      SELECT default_extension,fname,lname,displayname,title,company,department,work,fax,email,description,home
	      FROM userman_users
	      WHERE id
	      IN (?` + strings.Repeat(",?", len(usersid)-1) + `)
	    `

		rows, err := db.Query(query, usersid...)
		if err != nil {
			return sliceUsers, err
		}

		for rows.Next() {
			err := rows.Scan(&User.PhoneNumber, &User.FirstName, &User.LastName,
				&User.DisplayName, &User.Title, &User.Company,
				&User.Department, &User.WorkPhoneNumber, &User.MAC,
				&User.Email, &User.Description, &User.Home)
			if err != nil {
				return sliceUsers, err
			}
			User.GroupID = groupID
			User.GroupName = groupName
			sliceUsers = append(sliceUsers, User)
		}
	}

	return sliceUsers, nil
}

//Get PBX groups
func getPBXGroups() ([]Group, error) {
	var (
		group      Group
		groupslice []Group
	)

	query := `
	  SELECT id,groupname,description,users
	  FROM userman_groups
	  WHERE groupname
	  REGEXP 'pbx-phonebook.*'
	  `

	rows, err := db.Query(query)
	if err != nil {
		return []Group{}, err
	}

	defer rows.Close()

	for rows.Next() {
		err := rows.Scan(&group.ID, &group.GroupName, &group.Description, &group.Users)
		if err != nil {
			return []Group{}, err
		}
		groupslice = append(groupslice, group)
	}

	return groupslice, nil
}

//Get PBX users
func getPBXUsers(groups []Group) ([]ADuser, error) {
	var Users []ADuser

	for _, group := range groups {
		User, err := selectUsers(group.Users, group.ID, group.GroupName)
		if err != nil {
			return []ADuser{}, err
		}

		Users = append(Users, User...)
	}

	return Users, nil
}

//Parse PBX config file
func getDBConnectionParams() (string, error) {
	var con string

	rex := regexp.MustCompile(`\["(.*)"\] = "(.*)";`)
	buf := new(bytes.Buffer)

	file, err := os.Open(freepbxConf)
	if err != nil {
		return con, err
	}
	defer file.Close()

	buf.ReadFrom(file)

	data := rex.FindAllStringSubmatch(buf.String(), -1)

	res := make(map[string]string)
	for _, kv := range data {
		k := kv[1]
		v := kv[2]
		res[k] = v
	}

	con = fmt.Sprintf("%s:%s@tcp(%s)/%s", res["AMPDBUSER"], res["AMPDBPASS"], res["AMPDBHOST"], res["AMPDBNAME"])

	return con, nil
}

//Get local ip adress
func getIP() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue // interface down
		}
		if iface.Flags&net.FlagLoopback != 0 {
			continue // loopback interface
		}
		addrs, err := iface.Addrs()
		if err != nil {
			return "", err
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			ip = ip.To4()
			if ip == nil {
				continue // not an ipv4 address
			}
			return ip.String(), nil
		}
	}
	return "", nil
}

//Main func for generate all PhoneBooks
func generatePhoneBooks() error {

	fmt.Println("Updating phone books...")

	//Create working directory
	if _, err := os.Stat(workdir); os.IsNotExist(err) {
		os.Mkdir(workdir, 0755)
	}

	//Increment fot template
	increment := template.FuncMap{
		"inc": func(i int) int {
			return i + 1
		},
	}

	//Get PBX Groups
	groups, err := getPBXGroups()
	if err != nil {
		return err
	}

	//Get PBX users for per groups
	Users, err := getPBXUsers(groups)
	if err != nil {
		return err
	}

	//Create template
	allTemplates, err := template.New("phonebooks").Funcs(increment).ParseGlob(filepath.Join(templatesFileDir, "*"))
	if err != nil {
		return err
	}

	//Generate Cisco Phonebook
	err = ciscoPhonebook(allTemplates, groups, Users)
	if err != nil {
		return err
	}

	//Generate Grandstream Phonebook
	err = grandstreamPhonebook(allTemplates, groups, Users)
	if err != nil {
		return err
	}

	//Generate Website Phonebook
	err = websitePhonebook(allTemplates, groups, Users)
	if err != nil {
		return err
	}

	return nil
}

//Http loging
func logRequest(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Printf("%s %s %s\n", r.RemoteAddr, r.Method, r.URL)
		handler.ServeHTTP(w, r)
	})
}

func init() {
	flag.StringVar(&workdir, "workdir", "./www", "Set working directory")
	flag.StringVar(&templatesFileDir, "templates-file-dir", "./templates", "Set path to templates phonebook files")
	flag.StringVar(&freepbxConf, "freepbx-conf", "/etc/freepbx.conf", "Set path to freepbx db connection config file")
	flag.StringVar(&serveraddr, "server-addr", "", "Overwrite ip/dns name for template")
	flag.IntVar(&listenport, "listen-port", 8081, "Set http server listen port")
	flag.StringVar(&cron, "cron", "*/5 * * * *", "Set update time phone books")
}

func main() {

	var err error

	flag.Parse()

	//Detect ip if server-addr not set
	if serveraddr == "" {
		serveraddr, err = getIP()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to get ip address: %v\n", err)
			os.Exit(1)
		}
	}
	fmt.Printf("Server address: %s\n", serveraddr)

	//Getting params for db connection
	dbConnParams, err := getDBConnectionParams()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	//Connicting to db
	db, err = sql.Open("mysql", dbConnParams)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error on initializing database connection: %s\n", err)
		os.Exit(1)
	}

	db.SetMaxIdleConns(10)

	//Checking db connection
	err = db.Ping()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error on database connection: %s\n", err)
		os.Exit(1)
	}

	//Create working directory
	if _, err := os.Stat(workdir); os.IsNotExist(err) {
		os.Mkdir(workdir, 0755)
	}

	//Generating phone books
	err = generatePhoneBooks()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error generating phone books: %s\n", err)
		os.Exit(1)
	}

	//Create cron table
	ctab := crontab.New()
	err = ctab.AddJob(cron, generatePhoneBooks)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
	}

	http.Handle("/", http.FileServer(http.Dir(workdir)))

	fmt.Printf("Serving %s on HTTP port: %d\n", workdir, listenport)
	http.ListenAndServe(":"+strconv.Itoa(listenport), logRequest(http.DefaultServeMux))
}
