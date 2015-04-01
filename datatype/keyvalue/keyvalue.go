/*
	Package keyvalue implements DVID support for data using generic key-value.
*/
package keyvalue

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"

	"code.google.com/p/go.net/context"

	"github.com/janelia-flyem/dvid/datastore"
	"github.com/janelia-flyem/dvid/dvid"
	"github.com/janelia-flyem/dvid/message"
	"github.com/janelia-flyem/dvid/server"
	"github.com/janelia-flyem/dvid/storage"
)

const (
	Version  = "0.2"
	RepoURL  = "github.com/janelia-flyem/dvid/datatype/keyvalue"
	TypeName = "keyvalue"
)

const HelpMessage = `
API for 'keyvalue' datatype (github.com/janelia-flyem/dvid/datatype/keyvalue)
=============================================================================

Command-line:

$ dvid repo <UUID> new keyvalue <data name> <settings...>

	Adds newly named key-value data to repo with specified UUID.

	Example:

	$ dvid repo 3f8c new keyvalue stuff

    Arguments:

    UUID           Hexidecimal string with enough characters to uniquely identify a version node.
    data name      Name of data to create, e.g., "myblobs"
    settings       Configuration settings in "key=value" format separated by spaces.

$ dvid node <UUID> <data name> mount <directory>

	Creates a FUSE file system at given mount directory.  Each version will have
	a separate directory with the UUID as name.  Reading and writing files in this
	directory will be the same as reading and writing keyvalue data to DVID.
	
$ dvid -stdin node <UUID> <data name> put <key> < data

	Puts stdin data into the keyvalue data instance under the given key.

	
    ------------------

HTTP API (Level 2 REST):

Note that browsers support HTTP PUT and DELETE via javascript but only GET/POST are
included in HTML specs.  For ease of use in constructing clients, HTTP POST is used
to create or modify resources in an idempotent fashion.

GET  <api URL>/node/<UUID>/<data name>/help

	Returns data-specific help message.


GET  <api URL>/node/<UUID>/<data name>/info
POST <api URL>/node/<UUID>/<data name>/info

    Retrieves or puts data properties.

    Example: 

    GET <api URL>/node/3f8c/stuff/info

    Returns JSON with configuration settings.

    Arguments:

    UUID          Hexidecimal string with enough characters to uniquely identify a version node.
    data name     Name of keyvalue data instance.

GET  <api URL>/node/<UUID>/<data name>/keys

	Returns all keys for this data instance in JSON format:

	[key1, key2, ...]

GET  <api URL>/node/<UUID>/<data name>/keyrange/<key1>/<key2>

	Returns all keys between 'key1' and 'key2' for this data instance in JSON format:

	[key1, key2, ...]

    Arguments:

    UUID          Hexidecimal string with enough characters to uniquely identify a version node.
    data name     Name of keyvalue data instance.
    key1          First alphanumeric key in range.
    key2          Last alphanumeric key in range.

GET  <api URL>/node/<UUID>/<data name>/key/<key>
POST <api URL>/node/<UUID>/<data name>/key/<key>
DEL  <api URL>/node/<UUID>/<data name>/key/<key> 

    Performs operations on a key-value pair depending on the HTTP verb.  

    Example: 

    GET <api URL>/node/3f8c/stuff/key/myfile.dat

    Returns the data associated with the key "myfile.dat" of instance "stuff" in version
    node 3f8c.

    The "Content-type" of the HTTP response (and usually the request) are
    "application/octet-stream" for arbitrary binary data.

    Arguments:

    UUID          Hexidecimal string with enough characters to uniquely identify a version node.
    data name     Name of keyvalue data instance.
    key           An alphanumeric key.
`

func init() {
	datastore.Register(NewType())

	// Need to register types that will be used to fulfill interfaces.
	gob.Register(&Type{})
	gob.Register(&Data{})
}

// Type embeds the datastore's Type to create a unique type for keyvalue functions.
type Type struct {
	datastore.Type
}

// NewType returns a pointer to a new keyvalue Type with default values set.
func NewType() *Type {
	dtype := new(Type)
	dtype.Type = datastore.Type{
		Name:    TypeName,
		URL:     RepoURL,
		Version: Version,
		Requirements: &storage.Requirements{
			Batcher: true,
		},
	}
	return dtype
}

// --- TypeService interface ---

// NewData returns a pointer to new keyvalue data with default values.
func (dtype *Type) NewDataService(uuid dvid.UUID, id dvid.InstanceID, name dvid.InstanceName, c dvid.Config) (datastore.DataService, error) {
	basedata, err := datastore.NewDataService(dtype, uuid, id, name, c)
	if err != nil {
		return nil, err
	}
	return &Data{basedata}, nil
}

func (dtype *Type) Help() string {
	return fmt.Sprintf(HelpMessage)
}

// Data embeds the datastore's Data and extends it with keyvalue properties (none for now).
type Data struct {
	*datastore.Data
}

func (d *Data) Equals(d2 *Data) bool {
	if !d.Data.Equals(d2.Data) {
		return false
	}
	return true
}

func (d *Data) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Base     *datastore.Data
		Extended struct{}
	}{
		d.Data,
		struct{}{},
	})
}

func (d *Data) GobDecode(b []byte) error {
	buf := bytes.NewBuffer(b)
	dec := gob.NewDecoder(buf)
	if err := dec.Decode(&(d.Data)); err != nil {
		return err
	}
	return nil
}

func (d *Data) GobEncode() ([]byte, error) {
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(d.Data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (d *Data) GetKeysInRange(ctx storage.Context, keyBeg, keyEnd string) ([]string, error) {
	db, err := storage.BigDataStore()
	if err != nil {
		return nil, err
	}

	// Compute first and last key for range
	first, err := NewTKey(keyBeg)
	if err != nil {
		return nil, err
	}
	last, err := NewTKey(keyEnd)
	if err != nil {
		return nil, err
	}
	keys, err := db.KeysInRange(ctx, first, last)
	if err != nil {
		return nil, err
	}
	keyList := []string{}
	for _, key := range keys {
		keyStr, err := DecodeTKey(key)
		if err != nil {
			return nil, err
		}
		keyList = append(keyList, keyStr)
	}
	return keyList, nil
}

func (d *Data) GetKeys(ctx storage.Context) ([]string, error) {
	db, err := storage.BigDataStore()
	if err != nil {
		return nil, err
	}

	// Compute first and last key for range
	first := storage.MinTKey(keyStandard)
	last := storage.MaxTKey(keyStandard)
	keys, err := db.KeysInRange(ctx, first, last)
	if err != nil {
		return nil, err
	}
	keyList := []string{}
	for _, key := range keys {
		keyStr, err := DecodeTKey(key)
		if err != nil {
			return nil, err
		}
		keyList = append(keyList, keyStr)
	}
	return keyList, nil
}

// GetData gets a value using a key
func (d *Data) GetData(ctx storage.Context, keyStr string) ([]byte, bool, error) {
	db, err := storage.BigDataStore()
	if err != nil {
		return nil, false, err
	}
	tk, err := NewTKey(keyStr)
	if err != nil {
		return nil, false, err
	}
	data, err := db.Get(ctx, tk)
	if err != nil {
		return nil, false, fmt.Errorf("Error in retrieving key '%s': %s", keyStr, err.Error())
	}
	if data == nil {
		return nil, false, nil
	}
	uncompress := true
	value, _, err := dvid.DeserializeData(data, uncompress)
	if err != nil {
		return nil, false, fmt.Errorf("Unable to deserialize data for key '%s': %s\n", keyStr, err.Error())
	}
	return value, true, nil
}

// PutData puts a key-value at a given uuid
func (d *Data) PutData(ctx storage.Context, keyStr string, value []byte) error {
	db, err := storage.BigDataStore()
	if err != nil {
		return err
	}
	serialization, err := dvid.SerializeData(value, d.Compression(), d.Checksum())
	if err != nil {
		return fmt.Errorf("Unable to serialize data: %s\n", err.Error())
	}
	tk, err := NewTKey(keyStr)
	if err != nil {
		return err
	}
	return db.Put(ctx, tk, serialization)
}

// DeleteData deletes a key-value pair
func (d *Data) DeleteData(ctx storage.Context, keyStr string) error {
	db, err := storage.BigDataStore()
	if err != nil {
		return err
	}
	tk, err := NewTKey(keyStr)
	if err != nil {
		return err
	}
	return db.Delete(ctx, tk)
}

// put handles a PUT command-line request.
func (d *Data) put(cmd datastore.Request, reply *datastore.Response) error {
	if len(cmd.Command) < 5 {
		return fmt.Errorf("The key name must be specified after 'put'")
	}
	if len(cmd.Input) == 0 {
		return fmt.Errorf("No data was passed into standard input")
	}
	var uuidStr, dataName, cmdStr, keyStr string
	cmd.CommandArgs(1, &uuidStr, &dataName, &cmdStr, &keyStr)

	// Get repo.
	_, versionID, err := datastore.MatchingUUID(uuidStr)
	if err != nil {
		return err
	}

	// Store data
	ctx := datastore.NewVersionedContext(d, versionID)
	if err = d.PutData(ctx, keyStr, cmd.Input); err != nil {
		return fmt.Errorf("Error on put to key %q for keyvalue %q: %s\n", keyStr, d.DataName(),
			err.Error())
	}

	reply.Output = []byte(fmt.Sprintf("Put %d bytes into key %q for keyvalue %q, uuid %s\n",
		len(cmd.Input), keyStr, d.DataName(), uuidStr))
	return nil
}

// JSONString returns the JSON for this Data's configuration
func (d *Data) JSONString() (jsonStr string, err error) {
	m, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	return string(m), nil
}

// --- DataService interface ---

func (d *Data) Help() string {
	return fmt.Sprintf(HelpMessage)
}

// Send transfers all key-value pairs pertinent to this data type as well as
// the storage.DataStoreType for them.  For the keyvalue data type, ROI delimiting
// is not available.
func (d *Data) Send(s message.Socket, roiname string, uuid dvid.UUID) error {
	dvid.Criticalf("keyvalue.Send() is not implemented yet, so push/pull will not work for this data type.\n")
	return nil
}

// DoRPC acts as a switchboard for RPC commands.
func (d *Data) DoRPC(request datastore.Request, reply *datastore.Response) error {
	switch request.TypeCommand() {
	case "mount":
		return d.mount(request, reply)
	case "put":
		return d.put(request, reply)
	default:
		return fmt.Errorf("Unknown command.  Data '%s' [%s] does not support '%s' command.",
			d.DataName(), d.TypeName(), request.TypeCommand())
	}
	return nil
}

// ServeHTTP handles all incoming HTTP requests for this data.
func (d *Data) ServeHTTP(requestCtx context.Context, w http.ResponseWriter, r *http.Request) {
	timedLog := dvid.NewTimeLog()

	// Get repo and version ID of this request
	_, versions, err := datastore.FromContext(requestCtx)
	if err != nil {
		server.BadRequest(w, r, "Error: %q ServeHTTP has invalid context: %s\n", d.DataName, err.Error())
		return
	}

	// Construct storage.Context using a particular version of this Data
	var versionID dvid.VersionID
	if len(versions) > 0 {
		versionID = versions[0]
	}
	storeCtx := datastore.NewVersionedContext(d, versionID)

	// Break URL request into arguments
	url := r.URL.Path[len(server.WebAPIPath):]
	parts := strings.Split(url, "/")
	if len(parts[len(parts)-1]) == 0 {
		parts = parts[:len(parts)-1]
	}

	if len(parts) < 4 {
		server.BadRequest(w, r, "incomplete API specification")
		return
	}

	var comment string
	action := strings.ToLower(r.Method)

	switch parts[3] {
	case "help":
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, d.Help())
		return

	case "info":
		jsonStr, err := d.JSONString()
		if err != nil {
			server.BadRequest(w, r, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, jsonStr)
		return

	case "keys":
		keyList, err := d.GetKeys(storeCtx)
		if err != nil {
			server.BadRequest(w, r, err.Error())
			return
		}
		jsonBytes, err := json.Marshal(keyList)
		if err != nil {
			server.BadRequest(w, r, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, string(jsonBytes))
		comment = "HTTP GET keys"

	case "keyrange":
		// Return JSON list of keys
		keyBeg := parts[4]
		keyEnd := parts[5]
		keyList, err := d.GetKeysInRange(storeCtx, keyBeg, keyEnd)
		if err != nil {
			server.BadRequest(w, r, err.Error())
			return
		}
		jsonBytes, err := json.Marshal(keyList)
		if err != nil {
			server.BadRequest(w, r, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, string(jsonBytes))
		comment = fmt.Sprintf("HTTP GET keyrange [%q, %q]", keyBeg, keyEnd)

	case "key":
		keyStr := parts[4]

		switch action {
		case "get":
			// Return value of single key
			value, found, err := d.GetData(storeCtx, keyStr)
			if err != nil {
				server.BadRequest(w, r, err.Error())
				return
			}
			if !found {
				http.Error(w, fmt.Sprintf("Key %q not found", keyStr), http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			_, err = w.Write(value)
			if err != nil {
				server.BadRequest(w, r, err.Error())
				return
			}
			comment = fmt.Sprintf("HTTP GET key %q of keyvalue %q: %d bytes (%s)\n", keyStr, d.DataName(), len(value), url)

		case "delete":
			if err := d.DeleteData(storeCtx, keyStr); err != nil {
				server.BadRequest(w, r, err.Error())
				return
			}
			comment = fmt.Sprintf("HTTP DELETE data with key %q of keyvalue %q (%s)\n", keyStr, d.DataName(), url)

		case "post":
			data, err := ioutil.ReadAll(r.Body)
			if err != nil {
				server.BadRequest(w, r, err.Error())
				return
			}
			err = d.PutData(storeCtx, keyStr, data)
			if err != nil {
				server.BadRequest(w, r, err.Error())
				return
			}
			comment = fmt.Sprintf("HTTP POST keyvalue '%s': %d bytes (%s)\n", d.DataName(), len(data), url)
		default:
			server.BadRequest(w, r, "key endpoint does not support %q HTTP verb", action)
			return
		}

	default:
		server.BadRequest(w, r, "unknown action %q requested", parts[3])
		return
	}

	timedLog.Infof(comment)
}
