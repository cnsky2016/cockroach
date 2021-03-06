// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Peter Mattis (peter@cockroachlabs.com)

package sql

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"

	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/pkg/internal/client"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/privilege"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/pkg/errors"
)

type createDatabaseNode struct {
	p *planner
	n *parser.CreateDatabase
}

// CreateDatabase creates a database.
// Privileges: security.RootUser user.
//   Notes: postgres requires superuser or "CREATEDB".
//          mysql uses the mysqladmin command.
func (p *planner) CreateDatabase(n *parser.CreateDatabase) (planNode, error) {
	if n.Name == "" {
		return nil, errEmptyDatabaseName
	}

	if n.Template != nil {
		template, err := n.Template.ResolveAsType(&p.semaCtx, parser.TypeString)
		if err != nil {
			return nil, err
		}
		templateStr := string(*template.(*parser.DString))
		// See https://www.postgresql.org/docs/current/static/manage-ag-templatedbs.html
		if !strings.EqualFold(templateStr, "template0") {
			return nil, fmt.Errorf("unsupported template: %s", templateStr)
		}
	}

	if n.Encoding != nil {
		encoding, err := n.Encoding.ResolveAsType(&p.semaCtx, parser.TypeString)
		if err != nil {
			return nil, err
		}
		encodingStr := string(*encoding.(*parser.DString))
		// We only support UTF8 (and aliases for UTF8).
		if !(strings.EqualFold(encodingStr, "UTF8") ||
			strings.EqualFold(encodingStr, "UTF-8") ||
			strings.EqualFold(encodingStr, "UNICODE")) {
			return nil, fmt.Errorf("unsupported encoding: %s", encoding)
		}
	}

	if n.Collate != nil {
		collate, err := n.Collate.ResolveAsType(&p.semaCtx, parser.TypeString)
		if err != nil {
			return nil, err
		}
		collateStr := string(*collate.(*parser.DString))
		// We only support C and C.UTF-8.
		if collateStr != "C" && collateStr != "C.UTF-8" {
			return nil, fmt.Errorf("unsupported collation: %s", collate)
		}
	}

	if n.CType != nil {
		ctype, err := n.CType.ResolveAsType(&p.semaCtx, parser.TypeString)
		if err != nil {
			return nil, err
		}
		ctypeStr := string(*ctype.(*parser.DString))
		// We only support C and C.UTF-8.
		if ctypeStr != "C" && ctypeStr != "C.UTF-8" {
			return nil, fmt.Errorf("unsupported character classification: %s", ctype)
		}
	}

	if p.session.User != security.RootUser {
		return nil, fmt.Errorf("only %s is allowed to create databases", security.RootUser)
	}

	return &createDatabaseNode{p: p, n: n}, nil
}

func (n *createDatabaseNode) expandPlan() error {
	return nil
}

func (n *createDatabaseNode) Start() error {
	desc := makeDatabaseDesc(n.n)

	created, err := n.p.createDatabase(&desc, n.n.IfNotExists)
	if err != nil {
		return err
	}
	if created {
		// Log Create Database event. This is an auditable log event and is
		// recorded in the same transaction as the table descriptor update.
		if err := MakeEventLogger(n.p.leaseMgr).InsertEventRecord(n.p.txn,
			EventLogCreateDatabase,
			int32(desc.ID),
			int32(n.p.evalCtx.NodeID),
			struct {
				DatabaseName string
				Statement    string
				User         string
			}{n.n.Name.String(), n.n.String(), n.p.session.User},
		); err != nil {
			return err
		}
	}
	return nil
}

func (n *createDatabaseNode) Next() (bool, error)                 { return false, nil }
func (n *createDatabaseNode) Close()                              {}
func (n *createDatabaseNode) Columns() ResultColumns              { return make(ResultColumns, 0) }
func (n *createDatabaseNode) Ordering() orderingInfo              { return orderingInfo{} }
func (n *createDatabaseNode) Values() parser.DTuple               { return parser.DTuple{} }
func (n *createDatabaseNode) DebugValues() debugValues            { return debugValues{} }
func (n *createDatabaseNode) ExplainTypes(_ func(string, string)) {}
func (n *createDatabaseNode) SetLimitHint(_ int64, _ bool)        {}
func (n *createDatabaseNode) setNeededColumns(_ []bool)           {}
func (n *createDatabaseNode) MarkDebug(mode explainMode)          {}
func (n *createDatabaseNode) ExplainPlan(v bool) (string, string, []planNode) {
	return "create database", "", nil
}

type createIndexNode struct {
	p         *planner
	n         *parser.CreateIndex
	tableDesc *sqlbase.TableDescriptor
}

// CreateIndex creates an index.
// Privileges: CREATE on table.
//   notes: postgres requires CREATE on the table.
//          mysql requires INDEX on the table.
func (p *planner) CreateIndex(n *parser.CreateIndex) (planNode, error) {
	tn, err := n.Table.NormalizeWithDatabaseName(p.session.Database)
	if err != nil {
		return nil, err
	}

	tableDesc, err := p.mustGetTableDesc(tn)
	if err != nil {
		return nil, err
	}

	if err := p.checkPrivilege(tableDesc, privilege.CREATE); err != nil {
		return nil, err
	}

	return &createIndexNode{p: p, tableDesc: tableDesc, n: n}, nil
}

func (n *createIndexNode) expandPlan() error {
	return nil
}

func (n *createIndexNode) Start() error {
	status, i, err := n.tableDesc.FindIndexByName(n.n.Name)
	if err == nil {
		if status == sqlbase.DescriptorIncomplete {
			switch n.tableDesc.Mutations[i].Direction {
			case sqlbase.DescriptorMutation_DROP:
				return fmt.Errorf("index %q being dropped, try again later", string(n.n.Name))

			case sqlbase.DescriptorMutation_ADD:
				// Noop, will fail in AllocateIDs below.
			}
		}
		if n.n.IfNotExists {
			return nil
		}
	}

	indexDesc := sqlbase.IndexDescriptor{
		Name:             string(n.n.Name),
		Unique:           n.n.Unique,
		StoreColumnNames: n.n.Storing.ToStrings(),
	}
	if err := indexDesc.FillColumns(n.n.Columns); err != nil {
		return err
	}

	mutationIdx := len(n.tableDesc.Mutations)
	n.tableDesc.AddIndexMutation(indexDesc, sqlbase.DescriptorMutation_ADD)
	mutationID, err := n.tableDesc.FinalizeMutation()
	if err != nil {
		return err
	}
	if err := n.tableDesc.AllocateIDs(); err != nil {
		return err
	}

	if n.n.Interleave != nil {
		index := n.tableDesc.Mutations[mutationIdx].GetIndex()
		if err := n.p.addInterleave(n.tableDesc, index, n.n.Interleave); err != nil {
			return err
		}
		if err := n.p.finalizeInterleave(n.tableDesc, *index); err != nil {
			return err
		}
	}

	if err := n.p.txn.Put(
		sqlbase.MakeDescMetadataKey(n.tableDesc.GetID()),
		sqlbase.WrapDescriptor(n.tableDesc)); err != nil {
		return err
	}

	// Record index creation in the event log. This is an auditable log
	// event and is recorded in the same transaction as the table descriptor
	// update.
	if err := MakeEventLogger(n.p.leaseMgr).InsertEventRecord(n.p.txn,
		EventLogCreateIndex,
		int32(n.tableDesc.ID),
		int32(n.p.evalCtx.NodeID),
		struct {
			TableName  string
			IndexName  string
			Statement  string
			User       string
			MutationID uint32
		}{n.tableDesc.Name, n.n.Name.String(), n.n.String(), n.p.session.User, uint32(mutationID)},
	); err != nil {
		return err
	}
	n.p.notifySchemaChange(n.tableDesc.ID, mutationID)

	return nil
}

func (n *createIndexNode) Next() (bool, error)                 { return false, nil }
func (n *createIndexNode) Close()                              {}
func (n *createIndexNode) Columns() ResultColumns              { return make(ResultColumns, 0) }
func (n *createIndexNode) Ordering() orderingInfo              { return orderingInfo{} }
func (n *createIndexNode) Values() parser.DTuple               { return parser.DTuple{} }
func (n *createIndexNode) DebugValues() debugValues            { return debugValues{} }
func (n *createIndexNode) ExplainTypes(_ func(string, string)) {}
func (n *createIndexNode) SetLimitHint(_ int64, _ bool)        {}
func (n *createIndexNode) setNeededColumns(_ []bool)           {}
func (n *createIndexNode) MarkDebug(mode explainMode)          {}
func (n *createIndexNode) ExplainPlan(v bool) (string, string, []planNode) {
	return "create index", "", nil
}

type createUserNode struct {
	p        *planner
	n        *parser.CreateUser
	password string
}

// CreateUser creates a user.
// Privileges: INSERT on system.users.
//   notes: postgres allows the creation of users with an empty password. We do
//          as well, but disallow password authentication for these users.
func (p *planner) CreateUser(n *parser.CreateUser) (planNode, error) {
	if n.Name == "" {
		return nil, errors.New("no username specified")
	}

	tDesc, err := p.getTableDesc(&parser.TableName{DatabaseName: "system", TableName: "users"})
	if err != nil {
		return nil, err
	}

	if err := p.checkPrivilege(tDesc, privilege.INSERT); err != nil {
		return nil, err
	}

	var resolvedPassword string
	if n.Password != nil {
		password, err := n.Password.ResolveAsType(&p.semaCtx, parser.TypeString)
		if err != nil {
			return nil, err
		}

		resolvedPassword = string(*password.(*parser.DString))
		if resolvedPassword == "" {
			return nil, security.ErrEmptyPassword
		}
	}

	return &createUserNode{p: p, n: n, password: resolvedPassword}, nil
}

func (n *createUserNode) expandPlan() error {
	return nil
}

func (n *createUserNode) Start() error {
	var hashedPassword []byte
	if n.password != "" {
		var err error
		hashedPassword, err = security.HashPassword(n.password)
		if err != nil {
			return err
		}
	}

	normalizedUsername := n.n.Name.Normalize()

	internalExecutor := InternalExecutor{LeaseManager: n.p.leaseMgr}
	rowsAffected, err := internalExecutor.ExecuteStatementInTransaction(
		"create-user",
		n.p.txn,
		"INSERT INTO system.users VALUES ($1, $2);",
		normalizedUsername,
		hashedPassword,
	)
	if err != nil {
		if _, ok := err.(*sqlbase.ErrUniquenessConstraintViolation); ok {
			err = fmt.Errorf("user %s already exists", normalizedUsername)
		}

		return err
	} else if rowsAffected != 1 {
		return errors.Errorf(
			"%d rows affected by user creation; expected exactly one row affected", rowsAffected,
		)
	}

	return nil
}

func (n *createUserNode) Next() (bool, error)                 { return false, nil }
func (n *createUserNode) Close()                              {}
func (n *createUserNode) Columns() ResultColumns              { return make(ResultColumns, 0) }
func (n *createUserNode) Ordering() orderingInfo              { return orderingInfo{} }
func (n *createUserNode) Values() parser.DTuple               { return parser.DTuple{} }
func (n *createUserNode) DebugValues() debugValues            { return debugValues{} }
func (n *createUserNode) ExplainTypes(_ func(string, string)) {}
func (n *createUserNode) SetLimitHint(_ int64, _ bool)        {}
func (n *createUserNode) setNeededColumns(_ []bool)           {}
func (n *createUserNode) MarkDebug(mode explainMode)          {}
func (n *createUserNode) ExplainPlan(v bool) (string, string, []planNode) {
	return "create user", "", nil
}

type createViewNode struct {
	p          *planner
	n          *parser.CreateView
	dbDesc     *sqlbase.DatabaseDescriptor
	sourcePlan planNode
}

// CreateView creates a view.
// Privileges: CREATE on database plus SELECT on all the selected columns.
//   notes: postgres requires CREATE on database plus SELECT on all the
//						selected columns.
//          mysql requires CREATE VIEW plus SELECT on all the selected columns.
func (p *planner) CreateView(n *parser.CreateView) (planNode, error) {
	name, err := n.Name.NormalizeWithDatabaseName(p.session.Database)
	if err != nil {
		return nil, err
	}

	dbDesc, err := p.mustGetDatabaseDesc(name.Database())
	if err != nil {
		return nil, err
	}

	if err := p.checkPrivilege(dbDesc, privilege.CREATE); err != nil {
		return nil, err
	}

	// To avoid races with ongoing schema changes to tables that the view
	// depends on, make sure we use the most recent versions of table
	// descriptors rather than the copies in the lease cache.
	p.avoidCachedDescriptors = true
	sourcePlan, err := p.Select(n.AsSource, []parser.Type{}, false)
	if err != nil {
		return nil, err
	}
	numColNames := len(n.ColumnNames)
	numColumns := len(sourcePlan.Columns())
	if numColNames != 0 && numColNames != numColumns {
		sourcePlan.Close()
		return nil, sqlbase.NewSyntaxError(fmt.Sprintf(
			"CREATE VIEW specifies %d column name%s, but data source has %d column%s",
			numColNames, util.Pluralize(int64(numColNames)),
			numColumns, util.Pluralize(int64(numColumns))))
	}

	return &createViewNode{p: p, n: n, dbDesc: dbDesc, sourcePlan: sourcePlan}, nil
}

func (n *createViewNode) Start() error {
	tKey := tableKey{parentID: n.dbDesc.ID, name: n.n.Name.TableName().Table()}
	key := tKey.Key()
	if exists, err := n.p.descExists(key); err == nil && exists {
		// TODO(a-robinson): Support CREATE OR REPLACE commands.
		return sqlbase.NewRelationAlreadyExistsError(tKey.Name())
	} else if err != nil {
		return err
	}

	id, err := generateUniqueDescID(n.p.txn)
	if err != nil {
		return nil
	}

	// Inherit permissions from the database descriptor.
	privs := n.dbDesc.GetPrivileges()

	affected := make(map[sqlbase.ID]*sqlbase.TableDescriptor)
	desc, err := n.makeViewTableDesc(n.n, n.dbDesc.ID, id, n.sourcePlan.Columns(), privs, affected)
	if err != nil {
		return err
	}

	err = desc.ValidateTable()
	if err != nil {
		return err
	}

	err = n.p.createDescriptorWithID(key, id, &desc)
	if err != nil {
		return err
	}

	// Persist the back-references in all referenced table descriptors.
	for _, updated := range affected {
		if err := n.p.saveNonmutationAndNotify(updated); err != nil {
			return err
		}
	}
	if desc.Adding() {
		n.p.notifySchemaChange(desc.ID, sqlbase.InvalidMutationID)
	}
	if err := desc.Validate(n.p.txn); err != nil {
		return err
	}

	// Log Create View event. This is an auditable log event and is
	// recorded in the same transaction as the table descriptor update.
	if err := MakeEventLogger(n.p.leaseMgr).InsertEventRecord(n.p.txn,
		EventLogCreateView,
		int32(desc.ID),
		int32(n.p.evalCtx.NodeID),
		struct {
			ViewName  string
			Statement string
			User      string
		}{n.n.Name.String(), n.n.String(), n.p.session.User},
	); err != nil {
		return err
	}

	return nil
}

func (n *createViewNode) Close() {
	n.sourcePlan.Close()
	n.sourcePlan = nil
}

func (n *createViewNode) expandPlan() error                   { return n.sourcePlan.expandPlan() }
func (n *createViewNode) Next() (bool, error)                 { return false, nil }
func (n *createViewNode) Columns() ResultColumns              { return make(ResultColumns, 0) }
func (n *createViewNode) Ordering() orderingInfo              { return orderingInfo{} }
func (n *createViewNode) Values() parser.DTuple               { return parser.DTuple{} }
func (n *createViewNode) DebugValues() debugValues            { return debugValues{} }
func (n *createViewNode) ExplainTypes(_ func(string, string)) {}
func (n *createViewNode) setNeededColumns(_ []bool)           {}
func (n *createViewNode) SetLimitHint(_ int64, _ bool)        {}
func (n *createViewNode) MarkDebug(mode explainMode)          {}
func (n *createViewNode) ExplainPlan(v bool) (string, string, []planNode) {
	return "create view", "", nil
}

type createTableNode struct {
	p          *planner
	n          *parser.CreateTable
	dbDesc     *sqlbase.DatabaseDescriptor
	sourcePlan planNode
}

// CreateTable creates a table.
// Privileges: CREATE on database.
//   Notes: postgres/mysql require CREATE on database.
func (p *planner) CreateTable(n *parser.CreateTable) (planNode, error) {
	tn, err := n.Table.NormalizeWithDatabaseName(p.session.Database)
	if err != nil {
		return nil, err
	}

	dbDesc, err := p.mustGetDatabaseDesc(tn.Database())
	if err != nil {
		return nil, err
	}

	if err := p.checkPrivilege(dbDesc, privilege.CREATE); err != nil {
		return nil, err
	}

	hoistConstraints(n)
	for _, def := range n.Defs {
		switch t := def.(type) {
		case *parser.ForeignKeyConstraintTableDef:
			if _, err := t.Table.NormalizeWithDatabaseName(p.session.Database); err != nil {
				return nil, err
			}
		}
	}

	var sourcePlan planNode
	if n.As() {
		// The sourcePlan is needed to determine the set of columns to use
		// to populate the new table descriptor in Start() below. We
		// instantiate the sourcePlan as early as here so that EXPLAIN has
		// something useful to show about CREATE TABLE .. AS ...
		sourcePlan, err = p.Select(n.AsSource, []parser.Type{}, false)
		if err != nil {
			return nil, err
		}
		numColNames := len(n.AsColumnNames)
		numColumns := len(sourcePlan.Columns())
		if numColNames != 0 && numColNames != numColumns {
			sourcePlan.Close()
			return nil, sqlbase.NewSyntaxError(fmt.Sprintf(
				"CREATE TABLE specifies %d column name%s, but data source has %d column%s",
				numColNames, util.Pluralize(int64(numColNames)),
				numColumns, util.Pluralize(int64(numColumns))))
		}
	}

	return &createTableNode{p: p, n: n, dbDesc: dbDesc, sourcePlan: sourcePlan}, nil
}

func hoistConstraints(n *parser.CreateTable) {
	for _, d := range n.Defs {
		if col, ok := d.(*parser.ColumnTableDef); ok {
			for _, checkExpr := range col.CheckExprs {
				n.Defs = append(n.Defs,
					&parser.CheckConstraintTableDef{
						Expr: checkExpr.Expr,
						Name: checkExpr.ConstraintName,
					},
				)
			}
			col.CheckExprs = nil
			if col.HasFKConstraint() {
				var targetCol parser.NameList
				if col.References.Col != "" {
					targetCol = append(targetCol, col.References.Col)
				}
				n.Defs = append(n.Defs, &parser.ForeignKeyConstraintTableDef{
					Table:    col.References.Table,
					FromCols: parser.NameList{col.Name},
					ToCols:   targetCol,
					Name:     col.References.ConstraintName,
				})
				col.References.Table = parser.NormalizableTableName{}
			}
		}
	}
}

func (n *createTableNode) expandPlan() error {
	if n.sourcePlan != nil {
		return n.sourcePlan.expandPlan()
	}
	return nil
}

func (n *createTableNode) Start() error {
	tKey := tableKey{parentID: n.dbDesc.ID, name: n.n.Table.TableName().Table()}
	key := tKey.Key()
	if exists, err := n.p.descExists(key); err == nil && exists {
		if n.n.IfNotExists {
			return nil
		}
		return sqlbase.NewRelationAlreadyExistsError(tKey.Name())
	} else if err != nil {
		return err
	}

	id, err := generateUniqueDescID(n.p.txn)
	if err != nil {
		return err
	}

	privs := n.dbDesc.GetPrivileges()
	var desc sqlbase.TableDescriptor
	var affected map[sqlbase.ID]*sqlbase.TableDescriptor
	if n.n.As() {
		desc, err = makeTableDescIfAs(n.n, n.dbDesc.ID, id, n.sourcePlan.Columns(), privs)
	} else {
		affected = make(map[sqlbase.ID]*sqlbase.TableDescriptor)
		desc, err = n.p.makeTableDesc(n.n, n.dbDesc.ID, id, privs, affected)
	}
	if err != nil {
		return err
	}

	// We need to validate again after adding the FKs.
	// Only validate the table because backreferences aren't created yet.
	// Everything is validated below.
	err = desc.ValidateTable()
	if err != nil {
		return err
	}

	if err := n.p.createDescriptorWithID(key, id, &desc); err != nil {
		return err
	}

	for _, updated := range affected {
		if err := n.p.saveNonmutationAndNotify(updated); err != nil {
			return err
		}
	}
	if desc.Adding() {
		n.p.notifySchemaChange(desc.ID, sqlbase.InvalidMutationID)
	}

	for _, index := range desc.AllNonDropIndexes() {
		if len(index.Interleave.Ancestors) > 0 {
			if err := n.p.finalizeInterleave(&desc, index); err != nil {
				return err
			}
		}
	}

	if err := desc.Validate(n.p.txn); err != nil {
		return err
	}

	// Log Create Table event. This is an auditable log event and is
	// recorded in the same transaction as the table descriptor update.
	if err := MakeEventLogger(n.p.leaseMgr).InsertEventRecord(n.p.txn,
		EventLogCreateTable,
		int32(desc.ID),
		int32(n.p.evalCtx.NodeID),
		struct {
			TableName string
			Statement string
			User      string
		}{n.n.Table.String(), n.n.String(), n.p.session.User},
	); err != nil {
		return err
	}

	if n.n.As() {
		// TODO(knz): Ideally we would want to plug the sourcePlan which
		// was already computed as a data source into the insertNode. Now
		// unfortunately this is not so easy: when this point is reached,
		// sourcePlan.expandPlan() has already been called (for EXPLAIN),
		// and insertPlan.expandPlan() below would cause a 2nd invocation
		// and cause a panic. So instead we close this sourcePlan and let
		// the insertNode create it anew from the AsSource syntax node.
		n.sourcePlan.Close()
		n.sourcePlan = nil

		insert := &parser.Insert{Table: &n.n.Table, Rows: n.n.AsSource}
		insertPlan, err := n.p.Insert(insert, nil, false)
		if err != nil {
			return err
		}
		defer insertPlan.Close()
		if err := insertPlan.expandPlan(); err != nil {
			return err
		}
		if err = insertPlan.Start(); err != nil {
			return err
		}
		// This loop is done here instead of in the Next method
		// since CREATE TABLE is a DDL statement and Executor only
		// runs Next() for statements with type "Rows".
		for done := true; done; done, err = insertPlan.Next() {
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (n *createTableNode) Close() {
	if n.sourcePlan != nil {
		n.sourcePlan.Close()
		n.sourcePlan = nil
	}
}

func (n *createTableNode) Next() (bool, error)                 { return false, nil }
func (n *createTableNode) Columns() ResultColumns              { return make(ResultColumns, 0) }
func (n *createTableNode) Ordering() orderingInfo              { return orderingInfo{} }
func (n *createTableNode) Values() parser.DTuple               { return parser.DTuple{} }
func (n *createTableNode) DebugValues() debugValues            { return debugValues{} }
func (n *createTableNode) ExplainTypes(_ func(string, string)) {}
func (n *createTableNode) SetLimitHint(_ int64, _ bool)        {}
func (n *createTableNode) setNeededColumns(_ []bool)           {}
func (n *createTableNode) MarkDebug(mode explainMode)          {}
func (n *createTableNode) ExplainPlan(v bool) (string, string, []planNode) {
	if n.n.As() {
		return "create table", "create table as", []planNode{n.sourcePlan}
	}
	return "create table", "", nil
}

type indexMatch bool

const (
	matchExact  indexMatch = true
	matchPrefix indexMatch = false
)

// Referenced cols must be unique, thus referenced indexes must match exactly.
// Referencing cols have no uniqueness requirement and thus may match a strict
// prefix of an index.
func matchesIndex(
	cols []sqlbase.ColumnDescriptor, idx sqlbase.IndexDescriptor, exact indexMatch,
) bool {
	if len(cols) > len(idx.ColumnIDs) || (exact && len(cols) != len(idx.ColumnIDs)) {
		return false
	}

	for i := range cols {
		if cols[i].ID != idx.ColumnIDs[i] {
			return false
		}
	}
	return true
}

func (p *planner) resolveFK(
	tbl *sqlbase.TableDescriptor,
	d *parser.ForeignKeyConstraintTableDef,
	backrefs map[sqlbase.ID]*sqlbase.TableDescriptor,
	mode sqlbase.ConstraintValidity,
) error {
	return resolveFK(p.txn, &p.session.virtualSchemas, tbl, d, backrefs, mode)
}

// resolveFK looks up the tables and columns mentioned in a `REFERENCES`
// constraint and adds metadata representing that constraint to the descriptor.
// It may, in doing so, add to or alter descriptors in the passed in `backrefs`
// map of other tables that need to be updated when this table is created.
// Constraints that are not known to hold for existing data are created
// "unvalidated", but when table is empty (e.g. during creation), no existing
// data imples no existing violations, and thus the constraint can be created
// without the unvalidated flag.
func resolveFK(
	txn *client.Txn,
	vt VirtualTabler,
	tbl *sqlbase.TableDescriptor,
	d *parser.ForeignKeyConstraintTableDef,
	backrefs map[sqlbase.ID]*sqlbase.TableDescriptor,
	mode sqlbase.ConstraintValidity,
) error {
	targetTable := d.Table.TableName()
	target, err := getTableDesc(txn, vt, targetTable)
	if err != nil {
		return err
	}
	// Special-case: self-referencing FKs (i.e. referencing another col in the
	// same table) will reference a table name that doesn't exist yet (since we
	// are creating it).
	if target == nil {
		if targetTable.Table() == tbl.Name {
			target = tbl
		} else {
			return fmt.Errorf("referenced table %q not found", targetTable.String())
		}
	} else {
		// Since this FK is referencing another table, this table must be created in
		// a non-public "ADD" state and made public only after all leases on the
		// other table are updated to include the backref.
		if mode == sqlbase.ConstraintValidity_Validated {
			tbl.State = sqlbase.TableDescriptor_ADD
			if err := tbl.SetUpVersion(); err != nil {
				return err
			}
		}

		// If we resolve the same table more than once, we only want to edit a
		// single instance of it, so replace target with previously resolved table.
		if prev, ok := backrefs[target.ID]; ok {
			target = prev
		} else {
			backrefs[target.ID] = target
		}
	}

	srcCols, err := tbl.FindActiveColumnsByNames(d.FromCols)
	if err != nil {
		return err
	}

	targetColNames := d.ToCols
	// If no columns are specified, attempt to default to PK.
	if len(targetColNames) == 0 {
		targetColNames = make(parser.NameList, len(target.PrimaryIndex.ColumnNames))
		for i, n := range target.PrimaryIndex.ColumnNames {
			targetColNames[i] = parser.Name(n)
		}
	}

	targetCols, err := target.FindActiveColumnsByNames(targetColNames)
	if err != nil {
		return err
	}

	if len(targetCols) != len(srcCols) {
		return fmt.Errorf("%d columns must reference exactly %d columns in referenced table (found %d)",
			len(srcCols), len(srcCols), len(targetCols))
	}

	for i := range srcCols {
		if s, t := srcCols[i], targetCols[i]; s.Type.Kind != t.Type.Kind {
			return fmt.Errorf("type of %q (%s) does not match foreign key %q.%q (%s)",
				s.Name, s.Type.Kind, target.Name, t.Name, t.Type.Kind)
		}
	}

	constraintName := string(d.Name)
	if constraintName == "" {
		constraintName = fmt.Sprintf("fk_%s_ref_%s", d.FromCols[0], target.Name)
	}

	var targetIdx *sqlbase.IndexDescriptor
	if matchesIndex(targetCols, target.PrimaryIndex, matchExact) {
		targetIdx = &target.PrimaryIndex
	} else {
		found := false
		// Find the index corresponding to the referenced column.
		for i, idx := range target.Indexes {
			if idx.Unique && matchesIndex(targetCols, idx, matchExact) {
				targetIdx = &target.Indexes[i]
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("foreign key requires table %q have a unique index on %s", targetTable.String(), colNames(targetCols))
		}
	}

	ref := sqlbase.ForeignKeyReference{Table: target.ID, Index: targetIdx.ID, Name: constraintName}
	if mode == sqlbase.ConstraintValidity_Unvalidated {
		ref.Validity = sqlbase.ConstraintValidity_Unvalidated
	}
	backref := sqlbase.ForeignKeyReference{Table: tbl.ID}

	if matchesIndex(srcCols, tbl.PrimaryIndex, matchPrefix) {
		if tbl.PrimaryIndex.ForeignKey.IsSet() {
			return fmt.Errorf("columns cannot be used by multiple foreign key constraints")
		}
		tbl.PrimaryIndex.ForeignKey = ref
		backref.Index = tbl.PrimaryIndex.ID
	} else {
		found := false
		for i := range tbl.Indexes {
			if matchesIndex(srcCols, tbl.Indexes[i], matchPrefix) {
				if tbl.Indexes[i].ForeignKey.IsSet() {
					return fmt.Errorf("columns cannot be used by multiple foreign key constraints")
				}
				tbl.Indexes[i].ForeignKey = ref
				backref.Index = tbl.Indexes[i].ID
				found = true
				break
			}
		}
		if !found {
			added, err := addIndexForFK(tbl, srcCols, constraintName, ref)
			if err != nil {
				return err
			}
			backref.Index = added
		}
	}
	targetIdx.ReferencedBy = append(targetIdx.ReferencedBy, backref)
	return nil
}

// Adds an index to a table descriptor (that is in the process of being created)
// that will support using `srcCols` as the referencing (src) side of an FK.
func addIndexForFK(
	tbl *sqlbase.TableDescriptor,
	srcCols []sqlbase.ColumnDescriptor,
	constraintName string,
	ref sqlbase.ForeignKeyReference,
) (sqlbase.IndexID, error) {
	// No existing index for the referencing columns found, so we add one.
	idx := sqlbase.IndexDescriptor{
		Name:             fmt.Sprintf("%s_auto_index_%s", tbl.Name, constraintName),
		ColumnNames:      make([]string, len(srcCols)),
		ColumnDirections: make([]sqlbase.IndexDescriptor_Direction, len(srcCols)),
		ForeignKey:       ref,
	}
	for i, c := range srcCols {
		idx.ColumnDirections[i] = sqlbase.IndexDescriptor_ASC
		idx.ColumnNames[i] = c.Name
	}
	if err := tbl.AddIndex(idx, false); err != nil {
		return 0, err
	}
	if err := tbl.AllocateIDs(); err != nil {
		return 0, err
	}

	added := tbl.Indexes[len(tbl.Indexes)-1]

	// Since we just added the index, we can assume it is the last one rather than
	// searching all the indexes again. That said, we sanity check that it matches
	// in case a refactor ever violates that assumption.
	if !matchesIndex(srcCols, added, matchPrefix) {
		panic("no matching index and auto-generated index failed to match")
	}

	return added.ID, nil
}

// colNames converts a []colDesc to a human-readable string for use in error messages.
func colNames(cols []sqlbase.ColumnDescriptor) string {
	var s bytes.Buffer
	s.WriteString(`("`)
	for i, c := range cols {
		if i != 0 {
			s.WriteString(`", "`)
		}
		s.WriteString(c.Name)
	}
	s.WriteString(`")`)
	return s.String()
}

func (p *planner) saveNonmutationAndNotify(td *sqlbase.TableDescriptor) error {
	if err := td.SetUpVersion(); err != nil {
		return err
	}
	if err := td.ValidateTable(); err != nil {
		return err
	}
	if err := p.writeTableDesc(td); err != nil {
		return err
	}
	p.notifySchemaChange(td.ID, sqlbase.InvalidMutationID)
	return nil
}

func (p *planner) addInterleave(
	desc *sqlbase.TableDescriptor, index *sqlbase.IndexDescriptor, interleave *parser.InterleaveDef,
) error {
	return addInterleave(p.txn, &p.session.virtualSchemas, desc, index, interleave, p.session.Database)
}

// addInterleave marks an index as one that is interleaved in some parent data
// according to the given definition.
func addInterleave(
	txn *client.Txn,
	vt VirtualTabler,
	desc *sqlbase.TableDescriptor,
	index *sqlbase.IndexDescriptor,
	interleave *parser.InterleaveDef,
	sessionDB string,
) error {
	if interleave.DropBehavior != parser.DropDefault {
		return util.UnimplementedWithIssueErrorf(
			7854, "unsupported shorthand %s", interleave.DropBehavior)
	}

	tn, err := interleave.Parent.NormalizeWithDatabaseName(sessionDB)
	if err != nil {
		return err
	}

	parentTable, err := mustGetTableDesc(txn, vt, tn)
	if err != nil {
		return err
	}
	parentIndex := parentTable.PrimaryIndex

	if len(interleave.Fields) != len(parentIndex.ColumnIDs) {
		return fmt.Errorf("interleaved columns must match parent")
	}
	if len(interleave.Fields) > len(index.ColumnIDs) {
		return fmt.Errorf("declared columns must match index being interleaved")
	}
	for i, targetColID := range parentIndex.ColumnIDs {
		targetCol, err := parentTable.FindColumnByID(targetColID)
		if err != nil {
			return err
		}
		col, err := desc.FindColumnByID(index.ColumnIDs[i])
		if err != nil {
			return err
		}
		if interleave.Fields[i].Normalize() != parser.ReNormalizeName(col.Name) {
			return fmt.Errorf("declared columns must match index being interleaved")
		}
		if !reflect.DeepEqual(col.Type, targetCol.Type) ||
			index.ColumnDirections[i] != parentIndex.ColumnDirections[i] {

			return fmt.Errorf("interleaved columns must match parent")
		}
	}

	ancestorPrefix := append(
		[]sqlbase.InterleaveDescriptor_Ancestor(nil), parentIndex.Interleave.Ancestors...)
	intl := sqlbase.InterleaveDescriptor_Ancestor{
		TableID:         parentTable.ID,
		IndexID:         parentIndex.ID,
		SharedPrefixLen: uint32(len(parentIndex.ColumnIDs)),
	}
	for _, ancestor := range ancestorPrefix {
		intl.SharedPrefixLen -= ancestor.SharedPrefixLen
	}
	index.Interleave = sqlbase.InterleaveDescriptor{Ancestors: append(ancestorPrefix, intl)}

	desc.State = sqlbase.TableDescriptor_ADD
	return nil
}

// finalizeInterleave creats backreferences from an interleaving parent to the
// child data being interleaved.
func (p *planner) finalizeInterleave(
	desc *sqlbase.TableDescriptor, index sqlbase.IndexDescriptor,
) error {
	// TODO(dan): This is similar to finalizeFKs. Consolidate them
	if len(index.Interleave.Ancestors) == 0 {
		return nil
	}
	// Only the last ancestor needs the backreference.
	ancestor := index.Interleave.Ancestors[len(index.Interleave.Ancestors)-1]
	var ancestorTable *sqlbase.TableDescriptor
	if ancestor.TableID == desc.ID {
		ancestorTable = desc
	} else {
		var err error
		ancestorTable, err = sqlbase.GetTableDescFromID(p.txn, ancestor.TableID)
		if err != nil {
			return err
		}
	}
	ancestorIndex, err := ancestorTable.FindIndexByID(ancestor.IndexID)
	if err != nil {
		return err
	}
	ancestorIndex.InterleavedBy = append(ancestorIndex.InterleavedBy,
		sqlbase.ForeignKeyReference{Table: desc.ID, Index: index.ID})

	if err := p.saveNonmutationAndNotify(ancestorTable); err != nil {
		return err
	}

	if desc.State == sqlbase.TableDescriptor_ADD {
		desc.State = sqlbase.TableDescriptor_PUBLIC

		if err := p.saveNonmutationAndNotify(desc); err != nil {
			return err
		}
	}

	return nil
}

// makeViewTableDesc returns the table descriptor for a new view.
//
// It creates the descriptor directly in the PUBLIC state rather than
// the ADDING state because back-references are added to the view's
// dependencies in the same transaction that the view is created and it
// doesn't matter if reads/writes use a cached descriptor that doesn't
// include the back-references.
func (n *createViewNode) makeViewTableDesc(
	p *parser.CreateView,
	parentID sqlbase.ID,
	id sqlbase.ID,
	resultColumns []ResultColumn,
	privileges *sqlbase.PrivilegeDescriptor,
	affected map[sqlbase.ID]*sqlbase.TableDescriptor,
) (sqlbase.TableDescriptor, error) {
	desc := sqlbase.TableDescriptor{
		ID:            id,
		ParentID:      parentID,
		FormatVersion: sqlbase.FamilyFormatVersion,
		Version:       1,
		Privileges:    privileges,
	}
	viewName, err := p.Name.Normalize()
	if err != nil {
		return desc, err
	}
	desc.Name = viewName.Table()
	for i, colRes := range resultColumns {
		colType, _ := parser.DatumTypeToColumnType(colRes.Typ)
		columnTableDef := parser.ColumnTableDef{Name: parser.Name(colRes.Name), Type: colType}
		if len(p.ColumnNames) > i {
			columnTableDef.Name = p.ColumnNames[i]
		}
		// We pass an empty search path here because there are no names to resolve.
		col, _, err := sqlbase.MakeColumnDefDescs(&columnTableDef, nil)
		if err != nil {
			return desc, err
		}
		desc.AddColumn(*col)
	}

	// TODO(a-robinson): Support star expressions as soon as we can (#10028).
	if planContainsStar(n.sourcePlan) {
		return desc, fmt.Errorf("views do not currently support * expressions")
	}

	n.resolveViewDependencies(&desc, affected)

	var buf bytes.Buffer
	var fmtErr error
	p.AsSource.Format(&buf, parser.FmtNormalizeTableNames(
		func(t *parser.NormalizableTableName) *parser.TableName {
			tn, err := n.p.QualifyWithDatabase(t)
			if err != nil {
				log.Warningf(n.p.ctx(), "failed to qualify table name %q with database name: %v", t, err)
				fmtErr = err
				return nil
			}
			return tn
		}))
	if fmtErr != nil {
		return desc, fmtErr
	}
	desc.ViewQuery = buf.String()

	return desc, desc.AllocateIDs()
}

// makeTableDescIfAs is the MakeTableDesc method for when we have a table
// that is created with the CREATE AS format.
func makeTableDescIfAs(
	p *parser.CreateTable,
	parentID, id sqlbase.ID,
	resultColumns []ResultColumn,
	privileges *sqlbase.PrivilegeDescriptor,
) (sqlbase.TableDescriptor, error) {
	desc := sqlbase.TableDescriptor{
		ID:            id,
		ParentID:      parentID,
		FormatVersion: sqlbase.InterleavedFormatVersion,
		Version:       1,
		Privileges:    privileges,
	}
	tableName, err := p.Table.Normalize()
	if err != nil {
		return desc, err
	}
	desc.Name = tableName.Table()
	for i, colRes := range resultColumns {
		colType, _ := parser.DatumTypeToColumnType(colRes.Typ)
		columnTableDef := parser.ColumnTableDef{Name: parser.Name(colRes.Name), Type: colType}
		if len(p.AsColumnNames) > i {
			columnTableDef.Name = p.AsColumnNames[i]
		}
		// We pass an empty search path here because we do not have any expressions to resolve.
		col, _, err := sqlbase.MakeColumnDefDescs(&columnTableDef, nil)
		if err != nil {
			return desc, err
		}
		desc.AddColumn(*col)
	}

	return desc, desc.AllocateIDs()
}

// MakeTableDesc creates a table descriptor from a CreateTable statement.
func MakeTableDesc(
	txn *client.Txn,
	vt VirtualTabler,
	searchPath parser.SearchPath,
	n *parser.CreateTable,
	parentID, id sqlbase.ID,
	privileges *sqlbase.PrivilegeDescriptor,
	affected map[sqlbase.ID]*sqlbase.TableDescriptor,
	sessionDB string,
) (sqlbase.TableDescriptor, error) {
	desc := sqlbase.TableDescriptor{
		ID:            id,
		ParentID:      parentID,
		FormatVersion: sqlbase.InterleavedFormatVersion,
		Version:       1,
		Privileges:    privileges,
	}
	tableName, err := n.Table.Normalize()
	if err != nil {
		return desc, err
	}
	desc.Name = tableName.Table()

	for _, def := range n.Defs {
		if d, ok := def.(*parser.ColumnTableDef); ok {
			if !desc.IsVirtualTable() {
				if _, ok := d.Type.(*parser.ArrayColType); ok {
					return desc, util.UnimplementedWithIssueErrorf(2115, "ARRAY column types are unsupported")
				}
			}

			col, idx, err := sqlbase.MakeColumnDefDescs(d, searchPath)
			if err != nil {
				return desc, err
			}
			desc.AddColumn(*col)
			if idx != nil {
				if err := desc.AddIndex(*idx, d.PrimaryKey); err != nil {
					return desc, err
				}
			}
			if d.HasColumnFamily() {
				// Pass true for `create` and `ifNotExists` because when we're creating
				// a table, we always want to create the specified family if it doesn't
				// exist.
				err := desc.AddColumnToFamilyMaybeCreate(col.Name, string(d.Family.Name), true, true)
				if err != nil {
					return desc, err
				}
			}
		}
	}

	var primaryIndexColumnSet map[string]struct{}
	for _, def := range n.Defs {
		switch d := def.(type) {
		case *parser.ColumnTableDef:
			// pass, handled above.

		case *parser.IndexTableDef:
			idx := sqlbase.IndexDescriptor{
				Name:             string(d.Name),
				StoreColumnNames: d.Storing.ToStrings(),
			}
			if err := idx.FillColumns(d.Columns); err != nil {
				return desc, err
			}
			if err := desc.AddIndex(idx, false); err != nil {
				return desc, err
			}
			if d.Interleave != nil {
				return desc, util.UnimplementedWithIssueErrorf(9148, "use CREATE INDEX to make interleaved indexes")
			}
		case *parser.UniqueConstraintTableDef:
			idx := sqlbase.IndexDescriptor{
				Name:             string(d.Name),
				Unique:           true,
				StoreColumnNames: d.Storing.ToStrings(),
			}
			if err := idx.FillColumns(d.Columns); err != nil {
				return desc, err
			}
			if err := desc.AddIndex(idx, d.PrimaryKey); err != nil {
				return desc, err
			}
			if d.PrimaryKey {
				primaryIndexColumnSet = make(map[string]struct{})
				for _, c := range d.Columns {
					primaryIndexColumnSet[c.Column.Normalize()] = struct{}{}
				}
			}
			if d.Interleave != nil {
				return desc, util.UnimplementedWithIssueErrorf(9148, "use CREATE INDEX to make interleaved indexes")
			}

		case *parser.CheckConstraintTableDef, *parser.ForeignKeyConstraintTableDef, *parser.FamilyTableDef:
			// pass, handled below.

		default:
			return desc, errors.Errorf("unsupported table def: %T", def)
		}
	}

	if primaryIndexColumnSet != nil {
		// Primary index columns are not nullable.
		for i := range desc.Columns {
			if _, ok := primaryIndexColumnSet[parser.ReNormalizeName(desc.Columns[i].Name)]; ok {
				desc.Columns[i].Nullable = false
			}
		}
	}

	// Now that all columns are in place, add any explicit families (this is done
	// here, rather than in the constraint pass below since we want to pick up
	// explicit allocations before AllocateIDs adds implicit ones).
	for _, def := range n.Defs {
		if d, ok := def.(*parser.FamilyTableDef); ok {
			fam := sqlbase.ColumnFamilyDescriptor{
				Name:        string(d.Name),
				ColumnNames: d.Columns.ToStrings(),
			}
			desc.AddFamily(fam)
		}
	}

	if err := desc.AllocateIDs(); err != nil {
		return desc, err
	}

	if n.Interleave != nil {
		if err := addInterleave(txn, vt, &desc, &desc.PrimaryIndex, n.Interleave, sessionDB); err != nil {
			return desc, err
		}
	}

	// With all structural elements in place and IDs allocated, we can resolve the
	// constraints and qualifications.
	// FKs are resolved after the descriptor is otherwise complete and IDs have
	// been allocated since the FKs will reference those IDs. Resolution also
	// accumulates updates to other tables (adding backreferences) in the passed
	// map -- anything in that map should be saved when the table is created.
	generatedNames := map[string]struct{}{}
	for _, def := range n.Defs {
		switch d := def.(type) {
		case *parser.ColumnTableDef, *parser.IndexTableDef, *parser.UniqueConstraintTableDef, *parser.FamilyTableDef:
			// pass, handled above.

		case *parser.CheckConstraintTableDef:
			ck, err := makeCheckConstraint(desc, d, generatedNames, searchPath)
			if err != nil {
				return desc, err
			}
			desc.Checks = append(desc.Checks, ck)

		case *parser.ForeignKeyConstraintTableDef:
			err := resolveFK(txn, vt, &desc, d, affected, sqlbase.ConstraintValidity_Validated)
			if err != nil {
				return desc, err
			}
		default:
			return desc, errors.Errorf("unsupported table def: %T", def)
		}
	}

	// Multiple FKs from the same column would potentially result in ambiguous or
	// unexpected behavior with conflicting CASCADE/RESTRICT/etc behaviors.
	colsInFKs := make(map[sqlbase.ColumnID]struct{})
	for _, idx := range desc.Indexes {
		if idx.ForeignKey.IsSet() {
			for i := range idx.ColumnIDs {
				if _, ok := colsInFKs[idx.ColumnIDs[i]]; ok {
					return desc, fmt.Errorf(
						"column %q cannot be used by multiple foreign key constraints", idx.ColumnNames[i])
				}
				colsInFKs[idx.ColumnIDs[i]] = struct{}{}
			}
		}
	}

	return desc, desc.AllocateIDs()
}

// makeTableDesc creates a table descriptor from a CreateTable statement.
func (p *planner) makeTableDesc(
	n *parser.CreateTable,
	parentID, id sqlbase.ID,
	privileges *sqlbase.PrivilegeDescriptor,
	affected map[sqlbase.ID]*sqlbase.TableDescriptor,
) (sqlbase.TableDescriptor, error) {
	return MakeTableDesc(p.txn, &p.session.virtualSchemas, p.session.SearchPath, n, parentID, id, privileges, affected, p.session.Database)
}

// dummyColumnItem is used in makeCheckConstraint to construct an expression
// that can be both type-checked and examined for variable expressions.
type dummyColumnItem struct {
	typ parser.Type
}

// String implements the Stringer interface.
func (d dummyColumnItem) String() string {
	return fmt.Sprintf("<%s>", d.typ)
}

// Format implements the NodeFormatter interface.
func (d dummyColumnItem) Format(buf *bytes.Buffer, _ parser.FmtFlags) {
	buf.WriteString(d.String())
}

// Walk implements the Expr interface.
func (d dummyColumnItem) Walk(_ parser.Visitor) parser.Expr {
	return d
}

// TypeCheck implements the Expr interface.
func (d dummyColumnItem) TypeCheck(
	_ *parser.SemaContext, desired parser.Type,
) (parser.TypedExpr, error) {
	return d, nil
}

// Eval implements the TypedExpr interface.
func (dummyColumnItem) Eval(_ *parser.EvalContext) (parser.Datum, error) {
	panic("dummyColumnItem.Eval() is undefined")
}

// ResolvedType implements the TypedExpr interface.
func (d dummyColumnItem) ResolvedType() parser.Type {
	return d.typ
}

func makeCheckConstraint(
	desc sqlbase.TableDescriptor,
	d *parser.CheckConstraintTableDef,
	inuseNames map[string]struct{},
	searchPath parser.SearchPath,
) (*sqlbase.TableDescriptor_CheckConstraint, error) {
	// CHECK expressions seem to vary across databases. Wikipedia's entry on
	// Check_constraint (https://en.wikipedia.org/wiki/Check_constraint) says
	// that if the constraint refers to a single column only, it is possible to
	// specify the constraint as part of the column definition. Postgres allows
	// specifying them anywhere about any columns, but it moves all constraints to
	// the table level (i.e., columns never have a check constraint themselves). We
	// will adhere to the stricter definition.

	var nameBuf bytes.Buffer
	name := string(d.Name)

	generateName := name == ""
	if generateName {
		nameBuf.WriteString("check")
	}

	preFn := func(expr parser.Expr) (err error, recurse bool, newExpr parser.Expr) {
		vBase, ok := expr.(parser.VarName)
		if !ok {
			// Not a VarName, don't do anything to this node.
			return nil, true, expr
		}

		v, err := vBase.NormalizeVarName()
		if err != nil {
			return err, false, nil
		}

		c, ok := v.(*parser.ColumnItem)
		if !ok {
			return nil, true, expr
		}

		col, err := desc.FindActiveColumnByName(c.ColumnName)
		if err != nil {
			return fmt.Errorf("column %q not found for constraint %q",
				c.ColumnName, d.Expr.String()), false, nil
		}
		if generateName {
			nameBuf.WriteByte('_')
			nameBuf.WriteString(col.Name)
		}
		// Convert to a dummy node of the correct type.
		return nil, false, dummyColumnItem{col.Type.ToDatumType()}
	}

	expr, err := parser.SimpleVisit(d.Expr, preFn)
	if err != nil {
		return nil, err
	}

	var p parser.Parser
	if err := p.AssertNoAggregationOrWindowing(expr, "CHECK expressions", searchPath); err != nil {
		return nil, err
	}

	if err := sqlbase.SanitizeVarFreeExpr(expr, parser.TypeBool, "CHECK", searchPath); err != nil {
		return nil, err
	}
	if generateName {
		name = nameBuf.String()

		// If generated name isn't unique, attempt to add a number to the end to
		// get a unique name.
		if _, ok := inuseNames[name]; ok {
			i := 1
			for {
				appended := fmt.Sprintf("%s%d", name, i)
				if _, ok := inuseNames[appended]; !ok {
					name = appended
					break
				}
				i++
			}
		}
		if inuseNames != nil {
			inuseNames[name] = struct{}{}
		}
	}
	return &sqlbase.TableDescriptor_CheckConstraint{Expr: d.Expr.String(), Name: name}, nil
}

// CreateTestTableDescriptor converts a SQL string to a table for test purposes.
// Will fail on complex tables where that operation requires e.g. looking up
// other tables or otherwise utilizing a planner, since the planner used here is
// just a zero value placeholder.
func CreateTestTableDescriptor(
	parentID, id sqlbase.ID, schema string, privileges *sqlbase.PrivilegeDescriptor,
) (sqlbase.TableDescriptor, error) {
	stmt, err := parser.ParseOneTraditional(schema)
	if err != nil {
		return sqlbase.TableDescriptor{}, err
	}
	p := planner{session: &Session{context: context.Background()}}
	return p.makeTableDesc(stmt.(*parser.CreateTable), parentID, id, privileges, nil)
}

// resolveViewDependencies looks up the tables included in a view's query
// and adds metadata representing those dependencies to both the new view's
// descriptor and the dependend-upon tables' descriptors. The modified table
// descriptors are put into the backrefs map of other tables so that they can
// be updated when this view is created.
func (n *createViewNode) resolveViewDependencies(
	tbl *sqlbase.TableDescriptor, backrefs map[sqlbase.ID]*sqlbase.TableDescriptor,
) {
	// Add the necessary back-references to the descriptor for each referenced
	// table / view.
	populateViewBackrefs(n.sourcePlan, tbl, backrefs)

	// Also create the forward references in the new view's descriptor.
	tbl.DependsOn = make([]sqlbase.ID, 0, len(backrefs))
	for _, backref := range backrefs {
		tbl.DependsOn = append(tbl.DependsOn, backref.ID)
	}
}

func populateViewBackrefs(
	plan planNode, tbl *sqlbase.TableDescriptor, backrefs map[sqlbase.ID]*sqlbase.TableDescriptor,
) {
	// I was initially concerned about doing type assertions on every node in
	// the tree, but it's actually faster than a string comparison on the name
	// returned by ExplainPlan, judging by a mini-benchmark run on my laptop
	// with go 1.7.1.
	if sel, ok := plan.(*selectNode); ok {
		// If this is a view, we don't want to resolve the underlying scan(s).
		// We instead prefer to track the dependency on the view itself rather
		// than on its indirect dependencies.
		if sel.source.info.viewDesc != nil {
			populateViewBackrefFromViewDesc(sel.source.info.viewDesc, tbl, backrefs)
			// Return early to avoid processing the view's underlying query.
			return
		}
	} else if join, ok := plan.(*joinNode); ok {
		if join.left.info.viewDesc != nil {
			populateViewBackrefFromViewDesc(join.left.info.viewDesc, tbl, backrefs)
		} else {
			populateViewBackrefs(join.left.plan, tbl, backrefs)
		}
		if join.right.info.viewDesc != nil {
			populateViewBackrefFromViewDesc(join.right.info.viewDesc, tbl, backrefs)
		} else {
			populateViewBackrefs(join.right.plan, tbl, backrefs)
		}
		// Return early to avoid re-processing the children.
		return
	} else if scan, ok := plan.(*scanNode); ok {
		desc, ok := backrefs[scan.desc.ID]
		if !ok {
			desc = &scan.desc
			backrefs[desc.ID] = desc
		}
		ref := sqlbase.TableDescriptor_Reference{
			ID:        tbl.ID,
			ColumnIDs: make([]sqlbase.ColumnID, 0, len(scan.cols)),
		}
		if scan.specifiedIndex != nil {
			ref.IndexID = scan.specifiedIndex.ID
		}
		for i := range scan.cols {
			// Only include the columns that are actually needed.
			if scan.valNeededForCol[i] {
				ref.ColumnIDs = append(ref.ColumnIDs, scan.cols[i].ID)
			}
		}
		desc.DependedOnBy = append(desc.DependedOnBy, ref)
	}

	// We have to use the verbose version of ExplainPlan because the non-verbose
	// form skips over some layers of the tree (e.g. selectTopNode, selectNode).
	_, _, children := plan.ExplainPlan(true)
	for _, child := range children {
		populateViewBackrefs(child, tbl, backrefs)
	}
}

func populateViewBackrefFromViewDesc(
	dependency *sqlbase.TableDescriptor,
	tbl *sqlbase.TableDescriptor,
	backrefs map[sqlbase.ID]*sqlbase.TableDescriptor,
) {
	desc, ok := backrefs[dependency.ID]
	if !ok {
		desc = dependency
		backrefs[desc.ID] = desc
	}
	ref := sqlbase.TableDescriptor_Reference{ID: tbl.ID}
	desc.DependedOnBy = append(desc.DependedOnBy, ref)
}

func planContainsStar(plan planNode) bool {
	if sel, ok := plan.(*selectNode); ok {
		if sel.isStar {
			return true
		}
	}

	_, _, children := plan.ExplainPlan(true)
	for _, child := range children {
		if containsStar := planContainsStar(child); containsStar {
			return true
		}
	}
	return false
}
