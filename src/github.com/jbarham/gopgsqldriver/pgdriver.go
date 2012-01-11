// Copyright 2011 John E. Barham. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// pgsqldriver is a PostgreSQL driver for the experimental Go SQL database package.
package pgsqldriver

/*
#include <stdlib.h>
#include <libpq-fe.h>

static char**makeCharArray(int size) {
	return calloc(sizeof(char*), size);
}

static void setArrayString(char **a, char *s, int n) {
	a[n] = s;
}

static void freeCharArray(char **a, int size) {
	int i;
	for (i = 0; i < size; i++)
		free(a[i]);
	free(a);
}
*/
import "C"

import (
	"os"
	"fmt"
	"time"
	"runtime"
	"unsafe"
	"strings"
	"strconv"
	"encoding/hex"
	"exp/sql"
	"exp/sql/driver"
)

func connError(db *C.PGconn) os.Error {
	return os.NewError("conn error:" + C.GoString(C.PQerrorMessage(db)))
}

func resultError(res *C.PGresult) os.Error {
	serr := C.GoString(C.PQresultErrorMessage(res))
	if serr == "" {
		return nil
	}
	return os.NewError("result error: " + serr)
}

const timeFormat = "2006-01-02 15:04:05.000000-07"

type postgresDriver struct{}

// Open creates a new database connection using the given connection string.
// Each parameter setting is in the form 'keyword=value'.
// See http://www.postgresql.org/docs/9.0/static/libpq-connect.html#LIBPQ-PQCONNECTDBPARAMS
// for a list of recognized parameters.
func (d *postgresDriver) Open(name string) (conn driver.Conn, err os.Error) {
	cparams := C.CString(name)
	defer C.free(unsafe.Pointer(cparams))
	db := C.PQconnectdb(cparams)
	if C.PQstatus(db) != C.CONNECTION_OK {
		err = connError(db)
		C.PQfinish(db)
		return nil, err
	}
	conn = &driverConn{db, 0}
	runtime.SetFinalizer(conn, (*driverConn).Close)
	return
}

type driverConn struct {
	db      *C.PGconn
	stmtNum int
}

// Check that driverConn implements driver.Execer interface.
var _ driver.Execer = (*driverConn)(nil)

func (c *driverConn) exec(stmt string, args []interface{}) (cres *C.PGresult) {
	stmtstr := C.CString(stmt)
	defer C.free(unsafe.Pointer(stmtstr))
	if len(args) == 0 {
		cres = C.PQexec(c.db, stmtstr)
	} else {
		cargs := buildCArgs(args)
		defer C.freeCharArray(cargs, C.int(len(args)))
		cres = C.PQexecParams(c.db, stmtstr, C.int(len(args)), nil, cargs, nil, nil, 0)
	}
	return cres
}

func (c *driverConn) Exec(query string, args []interface{}) (res driver.Result, err os.Error) {
	cres := c.exec(query, args)
	if err = resultError(cres); err != nil {
		C.PQclear(cres)
		return
	}
	defer C.PQclear(cres)
	ns := C.GoString(C.PQcmdTuples(cres))
	if ns == "" {
		return driver.DDLSuccess, nil
	}
	rowsAffected, err := strconv.Atoi64(ns)
	if err != nil {
		return
	}
	return driver.RowsAffected(rowsAffected), nil
}

func (c *driverConn) Prepare(query string) (driver.Stmt, os.Error) {
	// Generate unique statement name.
	stmtname := strconv.Itoa(c.stmtNum)
	cstmtname := C.CString(stmtname)
	c.stmtNum++
	defer C.free(unsafe.Pointer(cstmtname))
	stmtstr := C.CString(query)
	defer C.free(unsafe.Pointer(stmtstr))
	res := C.PQprepare(c.db, cstmtname, stmtstr, 0, nil)
	err := resultError(res)
	if err != nil {
		C.PQclear(res)
		return nil, err
	}
	stmtinfo := C.PQdescribePrepared(c.db, cstmtname)
	err = resultError(stmtinfo)
	if err != nil {
		C.PQclear(stmtinfo)
		return nil, err
	}
	defer C.PQclear(stmtinfo)
	nparams := int(C.PQnparams(stmtinfo))
	statement := &driverStmt{stmtname, c.db, res, nparams}
	runtime.SetFinalizer(statement, (*driverStmt).Close)
	return statement, nil
}

func (c *driverConn) Close() os.Error {
	if c != nil && c.db != nil {
		C.PQfinish(c.db)
		c.db = nil
		runtime.SetFinalizer(c, nil)
	}
	return nil
}

func (c *driverConn) Begin() (driver.Tx, os.Error) {
	if _, err := c.Exec("BEGIN", nil); err != nil {
		return nil, err
	}
	// driverConn implements driver.Tx interface.
	return c, nil
}

func (c *driverConn) Commit() (err os.Error) {
	_, err = c.Exec("COMMIT", nil)
	return
}

func (c *driverConn) Rollback() (err os.Error) {
	_, err = c.Exec("ROLLBACK", nil)
	return
}

type driverStmt struct {
	name    string
	db      *C.PGconn
	res     *C.PGresult
	nparams int
}

func (s *driverStmt) NumInput() int {
	return s.nparams
}

func (s *driverStmt) exec(params []interface{}) *C.PGresult {
	stmtName := C.CString(s.name)
	defer C.free(unsafe.Pointer(stmtName))
	cparams := buildCArgs(params)
	defer C.freeCharArray(cparams, C.int(len(params)))
	return C.PQexecPrepared(s.db, stmtName, C.int(len(params)), cparams, nil, nil, 0)
}

func (s *driverStmt) Exec(args []interface{}) (res driver.Result, err os.Error) {
	cres := s.exec(args)
	if err = resultError(cres); err != nil {
		C.PQclear(cres)
		return
	}
	defer C.PQclear(cres)
	rowsAffected, err := strconv.Atoi64(C.GoString(C.PQcmdTuples(cres)))
	if err != nil {
		return
	}
	return driver.RowsAffected(rowsAffected), nil
}

func (s *driverStmt) Query(args []interface{}) (driver.Rows, os.Error) {
	cres := s.exec(args)
	if err := resultError(cres); err != nil {
		C.PQclear(cres)
		return nil, err
	}
	return newResult(cres), nil
}

func (s *driverStmt) Close() os.Error {
	if s != nil && s.res != nil {
		C.PQclear(s.res)
		runtime.SetFinalizer(s, nil)
	}
	return nil
}

type driverRows struct {
	res     *C.PGresult
	nrows   int
	currRow int
	ncols   int
	cols    []string
}

func newResult(res *C.PGresult) *driverRows {
	ncols := int(C.PQnfields(res))
	nrows := int(C.PQntuples(res))
	result := &driverRows{res: res, nrows: nrows, currRow: -1, ncols: ncols, cols: nil}
	runtime.SetFinalizer(result, (*driverRows).Close)
	return result
}

func (r *driverRows) Columns() []string {
	if r.cols == nil {
		r.cols = make([]string, r.ncols)
		for i := 0; i < r.ncols; i++ {
			r.cols[i] = C.GoString(C.PQfname(r.res, C.int(i)))
		}
	}
	return r.cols
}

func argErr(i int, argType string, err string) os.Error {
	return os.NewError(fmt.Sprintf("arg %d as %s: %s", i, argType, err))
}

func (r *driverRows) Next(dest []interface{}) os.Error {
	r.currRow++
	if r.currRow >= r.nrows {
		return os.EOF
	}

	for i := 0; i < len(dest); i++ {
		if int(C.PQgetisnull(r.res, C.int(r.currRow), C.int(i))) == 1 {
			dest[i] = nil
			continue
		}
		val := C.GoString(C.PQgetvalue(r.res, C.int(r.currRow), C.int(i)))
		switch vtype := uint(C.PQftype(r.res, C.int(i))); vtype {
		case BOOLOID:
			if val == "t" {
				dest[i] = "true"
			} else {
				dest[i] = "false"
			}
		case BYTEAOID:
			if !strings.HasPrefix(val, "\\x") {
				return argErr(i, "[]byte", "invalid byte string format")
			}
			buf, err := hex.DecodeString(val[2:])
			if err != nil {
				return argErr(i, "[]byte", err.String())
			}
			dest[i] = buf
		case CHAROID, BPCHAROID, VARCHAROID, TEXTOID,
			INT2OID, INT4OID, INT8OID, OIDOID, XIDOID,
			FLOAT8OID, FLOAT4OID,
			DATEOID, TIMEOID, TIMESTAMPOID, TIMESTAMPTZOID, INTERVALOID, TIMETZOID,
			NUMERICOID:
			dest[i] = val
		default:
			return os.NewError(fmt.Sprintf("unsupported type oid: %d", vtype))
		}
	}
	return nil
}

func (r *driverRows) Close() os.Error {
	if r.res != nil {
		C.PQclear(r.res)
		r.res = nil
		runtime.SetFinalizer(r, nil)
	}
	return nil
}

func buildCArgs(params []interface{}) **C.char {
	sparams := make([]string, len(params))
	for i, v := range params {
		var str string
		switch v := v.(type) {
		case []byte:
			str = "\\x" + hex.EncodeToString(v)
		case bool:
			if v {
				str = "t"
			} else {
				str = "f"
			}
		case *time.Time:
			str = v.Format(timeFormat)
		default:
			str = fmt.Sprint(v)
		}

		sparams[i] = str
	}
	cparams := C.makeCharArray(C.int(len(sparams)))
	for i, s := range sparams {
		C.setArrayString(cparams, C.CString(s), C.int(i))
	}
	return cparams
}

func init() {
	sql.Register("postgres", &postgresDriver{})
}