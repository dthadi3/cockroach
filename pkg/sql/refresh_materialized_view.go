// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package sql

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/tabledesc"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgnotice"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
)

type refreshMaterializedViewNode struct {
	n    *tree.RefreshMaterializedView
	desc *tabledesc.Mutable
}

func (p *planner) RefreshMaterializedView(
	ctx context.Context, n *tree.RefreshMaterializedView,
) (planNode, error) {
	if !p.EvalContext().TxnImplicit {
		return nil, pgerror.Newf(pgcode.InvalidTransactionState, "cannot refresh view in an explicit transaction")
	}
	desc, err := p.ResolveMutableTableDescriptorEx(ctx, n.Name, true /* required */, tree.ResolveRequireViewDesc)
	if err != nil {
		return nil, err
	}
	if !desc.MaterializedView() {
		return nil, pgerror.Newf(pgcode.WrongObjectType, "%q is not a materialized view", desc.Name)
	}
	// TODO (rohany): Not sure if this is a real restriction, but let's start with
	//  it to be safe.
	for i := range desc.Mutations {
		mut := &desc.Mutations[i]
		if mut.GetMaterializedViewRefresh() != nil {
			return nil, pgerror.Newf(pgcode.ObjectNotInPrerequisiteState, "view is already being refreshed")
		}
	}
	return &refreshMaterializedViewNode{n: n, desc: desc}, nil
}

func (n *refreshMaterializedViewNode) startExec(params runParams) error {
	// We refresh a materialized view by creating a new set of indexes to write
	// the result of the view query into. The existing set of indexes will remain
	// present and readable so that reads of the view during the refresh operation
	// will return consistent data. The schema change process will backfill the
	// results of the view query into the new set of indexes, and then change the
	// set of indexes over to the new set of indexes atomically.

	// Inform the user that CONCURRENTLY is not needed.
	if n.n.Concurrently {
		params.p.SendClientNotice(
			params.ctx,
			pgnotice.Newf("CONCURRENTLY is not required as views are refreshed concurrently"),
		)
	}

	// Prepare the new set of indexes by cloning all existing indexes on the view.
	newPrimaryIndex := protoutil.Clone(&n.desc.PrimaryIndex).(*descpb.IndexDescriptor)
	newIndexes := make([]descpb.IndexDescriptor, len(n.desc.Indexes))
	for i := range n.desc.Indexes {
		newIndexes[i] = *protoutil.Clone(&n.desc.Indexes[i]).(*descpb.IndexDescriptor)
	}

	// Reset and allocate new IDs for the new indexes.
	getID := func() descpb.IndexID {
		res := n.desc.NextIndexID
		n.desc.NextIndexID++
		return res
	}
	newPrimaryIndex.ID = getID()
	for i := range newIndexes {
		newIndexes[i].ID = getID()
	}

	// Queue the refresh mutation.
	n.desc.AddMaterializedViewRefreshMutation(&descpb.MaterializedViewRefresh{
		NewPrimaryIndex: *newPrimaryIndex,
		NewIndexes:      newIndexes,
		AsOf:            params.p.Txn().ReadTimestamp(),
	})

	return params.p.writeSchemaChange(
		params.ctx,
		n.desc,
		n.desc.ClusterVersion.NextMutationID,
		tree.AsStringWithFQNames(n.n, params.Ann()),
	)
}

func (n *refreshMaterializedViewNode) Next(params runParams) (bool, error) { return false, nil }
func (n *refreshMaterializedViewNode) Values() tree.Datums                 { return tree.Datums{} }
func (n *refreshMaterializedViewNode) Close(ctx context.Context)           {}
func (n *refreshMaterializedViewNode) ReadingOwnWrites()                   {}
