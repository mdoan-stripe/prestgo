package prestgo

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Name of the driver to use when calling `sql.Open`
const DriverName = "prestgo"

// Default data source parameters
const (
	DefaultPort     = "8080"
	DefaultCatalog  = "hive"
	DefaultSchema   = "default"
	DefaultUsername = "prestgo"

	TimestampFormat = "2006-01-02 15:04:05.000"
	DateFormat      = "2006-01-02"
)

var (
	// ErrNotSupported is returned when an unsupported feature is requested.
	ErrNotSupported = errors.New(DriverName + ": not supported")

	// ErrQueryFailed indicates that a network or server failure prevented the driver obtaining a query result.
	ErrQueryFailed = errors.New(DriverName + ": query failed")

	// ErrQueryCanceled indicates that a query was canceled before results could be retrieved.
	ErrQueryCanceled = errors.New(DriverName + ": query canceled")
)

func init() {
	sql.Register(DriverName, &drv{})
}

type drv struct{}

func (*drv) Open(name string) (driver.Conn, error) {
	return Open(name)
}

// Open creates a connection to the specified data source name which should be
// of the form "presto://hostname:port/catalog/schema?source=x&session=y". http.DefaultClient will
// be used for communicating with the Presto server.
func Open(name string) (driver.Conn, error) {
	return ClientOpen(http.DefaultClient, name)
}

// ClientOpen creates a connection to the specified data source name using the supplied
// HTTP client. The data source name should be of the form
// "presto://hostname:port/catalog/schema?source=x&session=y".
func ClientOpen(client *http.Client, name string) (driver.Conn, error) {

	conf := make(config)
	conf.parseDataSource(name)

	cn := &conn{
		client:  client,
		addr:    conf["addr"],
		catalog: conf["catalog"],
		schema:  conf["schema"],
		user:    conf["user"],
		source:  conf["source"],
		session:  conf["session"],
	}
	return cn, nil
}

type conn struct {
	client  *http.Client
	addr    string
	catalog string
	schema  string
	user    string
	source  string
	session string
}

var _ driver.Conn = &conn{}

func (c *conn) Prepare(query string) (driver.Stmt, error) {
	st := &stmt{
		conn:  c,
		query: query,
	}
	return st, nil
}

func (c *conn) Close() error {
	return nil
}

func (c *conn) Begin() (driver.Tx, error) {
	return nil, ErrNotSupported
}

type stmt struct {
	conn  *conn
	query string
}

var _ driver.Stmt = &stmt{}

func (s *stmt) Close() error {
	return nil
}

func (s *stmt) NumInput() int {
	return -1 // TODO: parse query for parameters
}

func (s *stmt) Exec(args []driver.Value) (driver.Result, error) {
	return nil, ErrNotSupported
}

func (s *stmt) Query(args []driver.Value) (driver.Rows, error) {
	// TODO: support query argument substitution
	if len(args) > 0 {
		return nil, ErrNotSupported
	}
	queryURL := fmt.Sprintf("http://%s/v1/statement", s.conn.addr)

	req, err := http.NewRequest("POST", queryURL, strings.NewReader(s.query))
	if err != nil {
		return nil, err
	}
	req.Header.Add("X-Presto-User", s.conn.user)
	req.Header.Add("X-Presto-Catalog", s.conn.catalog)
	req.Header.Add("X-Presto-Schema", s.conn.schema)
	if s.conn.source != "" {
		req.Header.Add("X-Presto-Source", s.conn.source)
	}
	if s.conn.session != "" {
		req.Header.Add("X-Presto-Session", s.conn.session)
	}

	resp, err := s.conn.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Presto doesn't use the http response code, parse errors come back as 200
	if resp.StatusCode != 200 {
		return nil, ErrQueryFailed
	}

	var sresp stmtResponse
	err = json.NewDecoder(resp.Body).Decode(&sresp)
	if err != nil {
		return nil, err
	}

	if sresp.Stats.State == "FAILED" {
		return nil, sresp.Error
	}

	time.Sleep(500 * time.Millisecond)

	r := &rows{
		conn:    s.conn,
		nextURI: sresp.NextURI,
	}

	return r, nil
}

type rows struct {
	conn     *conn
	nextURI  string
	fetched  bool
	rowindex int
	columns  []string
	types    []driver.ValueConverter
	data     []queryData
}

var _ driver.Rows = &rows{}

func (r *rows) fetch() error {
	// TODO: timeout
	for {
		qresp, gotData, err := r.waitForData()
		if err != nil {
			return err
		}
		if !gotData {
			time.Sleep(800 * time.Millisecond) // TODO: make this interval configurable
			continue
		}

		r.rowindex = 0
		r.data = qresp.Data

		// Note: qresp.Stats.State will be FINISHED when last page is retrieved
		r.nextURI = qresp.NextURI

		if !r.fetched {
			r.columns = make([]string, len(qresp.Columns))
			r.types = make([]driver.ValueConverter, len(qresp.Columns))
			for i, col := range qresp.Columns {
				r.columns[i] = col.Name
				switch {
				case strings.HasPrefix(col.Type, Row):
					// If the column is an unflattened struct, interpret as a string.
					r.types[i] = rowConverter{Type: col.Type}
				case strings.HasPrefix(col.Type, VarChar), strings.HasPrefix(col.Type, Char):
					r.types[i] = stringConverter
				case col.Type == JSON:
					// use string for json
					r.types[i] = stringConverter
				case col.Type == BigInt, col.Type == Integer, col.Type == Smallint, col.Type == Tinyint:
					r.types[i] = bigIntConverter
				case col.Type == Boolean:
					r.types[i] = boolConverter
				case col.Type == Double, col.Type == Real:
					r.types[i] = doubleConverter
				case strings.HasPrefix(col.Type, Decimal):
					// use string converter for this so that we keep our preciseness
					r.types[i] = stringConverter
				case col.Type == Date:
					r.types[i] = dateConverter
				case col.Type == Time:
					// use string here, having no date makes timestamps weird
					r.types[i] = stringConverter
				case col.Type == TimeWithTimezone:
					// use string here, having no date makes timestamps weird
					r.types[i] = stringConverter
				case col.Type == Timestamp:
					r.types[i] = timestampConverter
				case col.Type == TimestampWithTimezone:
					r.types[i] = timestampWithTimezoneConverter
				default:
					r.types[i] = stringConverter
					fmt.Println(fmt.Sprintf("unsupported column type: %s", col.Type))
				}
			}
			r.fetched = true
		}

		if len(qresp.Data) == 0 {
			return io.EOF
		}

		return nil
	}
}

func (r *rows) waitForData() (*queryResponse, bool, error) {
	nextReq, err := http.NewRequest("GET", r.nextURI, nil)
	if err != nil {
		return nil, false, err
	}

	nextResp, err := r.conn.client.Do(nextReq)
	if err != nil {
		return nil, false, err
	}

	if nextResp.StatusCode != 200 {
		nextResp.Body.Close()
		return nil, false, ErrQueryFailed
	}

	var qresp queryResponse
	err = json.NewDecoder(nextResp.Body).Decode(&qresp)
	nextResp.Body.Close()
	if err != nil {
		return nil, false, err
	}

	switch qresp.Stats.State {
	case QueryStateFailed:
		return nil, false, qresp.Error
	case QueryStateCanceled:
		return nil, false, ErrQueryCanceled
	case QueryStatePlanning, QueryStateQueued, QueryStateRunning, QueryStateStarting:
		if len(qresp.Data) == 0 {
			r.nextURI = qresp.NextURI
			return nil, false, nil
		}
	}

	return &qresp, true, nil
}

func (r *rows) Columns() []string {
	if !r.fetched {
		if err := r.fetch(); err != nil {
			return []string{}
		}
	}
	return r.columns
}

func (r *rows) Close() error {
	return nil
}

func (r *rows) Next(dest []driver.Value) error {
	if !r.fetched || r.rowindex >= len(r.data) {
		if r.nextURI == "" {
			return io.EOF
		}
		if err := r.fetch(); err != nil {
			return err
		}
	}

	for i, v := range r.types {
		val, err := v.ConvertValue(r.data[r.rowindex][i])
		if err != nil {
			return err // TODO: more context in error
		}
		dest[i] = val
	}
	r.rowindex++
	return nil
}

type config map[string]string

func (c config) parseDataSource(ds string) error {
	u, err := url.Parse(ds)
	if err != nil {
		return err
	}

	if u.User != nil {
		c["user"] = u.User.Username()
	} else {
		c["user"] = DefaultUsername
	}

	if strings.IndexRune(u.Host, ':') == -1 {
		c["addr"] = u.Host + ":" + DefaultPort
	} else {
		c["addr"] = u.Host
	}

	c["catalog"] = DefaultCatalog
	c["schema"] = DefaultSchema


	pathSegments := strings.FieldsFunc(u.Path, func(c rune) bool { return c == '/' })
	if len(pathSegments) > 0 {
		c["catalog"] = pathSegments[0]
	}
	if len(pathSegments) > 1 {
		c["schema"] = pathSegments[1]
	}

	m, _ := url.ParseQuery(u.RawQuery)
	for k, v := range m {
		c[k] = strings.Join(v, ",")
	}
	return nil
}

type valueConverterFunc func(v interface{}) (driver.Value, error)

func (fn valueConverterFunc) ConvertValue(v interface{}) (driver.Value, error) {
	return fn(v)
}

/** Stripe's (Data Platform) custom row converter
 * Hack: We introduce a custom class that converts unflattened structs in Presto into a JSON string.
 */
type rowConverter struct {
	Type string
}

func (rc rowConverter) ConvertValue(v interface{}) (driver.Value, error) {
	if v == nil {
		return nil, nil
	}
	// TODO: Write a custom parser to combine "rc.Type" and "v" into something like:
	// {_id="dp_9uVcPMp305RgYo",created=1484119972.0129445,open=false,...}
	return v, nil
}

var stringConverter = valueConverterFunc(func(val interface{}) (driver.Value, error) {
	if val == nil {
		return nil, nil
	}
	return driver.String.ConvertValue(val)
})

var boolConverter = valueConverterFunc(func(val interface{}) (driver.Value, error) {
	if val == nil {
		return nil, nil
	}
	return driver.Bool.ConvertValue(val)
})

// bigIntConverter converts a value from the underlying json response into an int64.
// The Go JSON decoder uses float64 for generic numeric values
var bigIntConverter = valueConverterFunc(func(val interface{}) (driver.Value, error) {
	if val == nil {
		return nil, nil
	}

	if vv, ok := val.(float64); ok {
		return int64(vv), nil
	}
	return nil, fmt.Errorf("%s: failed to convert %v (%T) into type int64", DriverName, val, val)
})

// doubleConverter converts a value from the underlying json response into an int64.
// The Go JSON decoder uses float64 for generic numeric values
var doubleConverter = valueConverterFunc(func(val interface{}) (driver.Value, error) {
	if val == nil {
		return nil, nil
	}

	switch vv := val.(type) {
	case float64:
		return vv, nil
	case string:
		switch vv {
		case "Infinity":
			return math.Inf(1), nil
		case "-Infinity":
			return math.Inf(-1), nil
		case "NaN":
			return math.NaN(), nil
		}

	}
	return nil, fmt.Errorf("%s: failed to convert %v (%T) into type float64", DriverName, val, val)
})

// dateConverter converts a value from the underlying json response into a time.Time.
var dateConverter = valueConverterFunc(func(val interface{}) (driver.Value, error) {
	if val == nil {
		return nil, nil
	}
	if vv, ok := val.(string); ok {
		// BUG: should parse using session time zone.
		if ts, err := time.ParseInLocation(DateFormat, vv, time.UTC); err == nil {
			return ts, nil
		}
	}
	return nil, fmt.Errorf("%s: failed to convert %v (%T) into type time.Time", DriverName, val, val)
})

// timestampConverter converts a value from the underlying json response into a time.Time.
var timestampConverter = valueConverterFunc(func(val interface{}) (driver.Value, error) {
	if val == nil {
		return nil, nil
	}
	if vv, ok := val.(string); ok {
		// BUG: should parse using session time zone.
		if ts, err := time.ParseInLocation(TimestampFormat, vv, time.UTC); err == nil {
			return ts, nil
		}
	}
	return nil, fmt.Errorf("%s: failed to convert %v (%T) into type time.Time", DriverName, val, val)
})

// timestampWithTimezoneConverter converts a value from the underlying json response into a time.Time including timezone.
var timestampWithTimezoneConverter = valueConverterFunc(func(val interface{}) (driver.Value, error) {
	if val == nil {
		return nil, nil
	}
	if vv, ok := val.(string); ok {
		if len(vv) <= len(TimestampFormat) {
			return timestampConverter(val)
		}
		tzOffset := strings.LastIndex(vv, " ")
		if tzOffset == -1 {
			return timestampConverter(val)
		}
		tz, err := time.LoadLocation(strings.TrimSpace(vv[tzOffset:]))
		if err != nil {
			return nil, err
		}
		ts, err := time.ParseInLocation(TimestampFormat, vv[:tzOffset], tz)
		if err != nil {
			return nil, err
		}
		return ts, nil
	}
	return nil, fmt.Errorf("%s: failed to convert %v (%T) into type time.Time", DriverName, val, val)
})
