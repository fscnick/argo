package controller

import (
	"fmt"

	wfv1 "github.com/argoproj/argo/api/workflow/v1"
	"github.com/argoproj/argo/errors"
	log "github.com/sirupsen/logrus"
)

// wfOperationCtx is the context for evaluation and operation of a single workflow
type wfOperationCtx struct {
	// wf is the workflow object
	wf *wfv1.Workflow
	// updated indicates whether or not the workflow object itself was updated
	// and needs to be persisted back to kubernetes
	updated bool
	// log is an logrus logging context to corrolate logs with a workflow
	log *log.Entry
	// controller reference to workflow controller
	controller *WorkflowController
	// NOTE: eventually we may need to store additional metadata state to
	// understand how to proceed in workflows with more complex control flows.
	// (e.g. workflow failed in step 1 of 3 but has finalizer steps)
}

// operateWorkflow is the operator logic of a workflow
// It evaluates the current state of the workflow and decides how to proceed down the execution path
func (wfc *WorkflowController) operateWorkflow(wf *wfv1.Workflow) {
	if wf.Completed() {
		return
	}
	// NEVER modify objects from the store. It's a read-only, local cache.
	// You can use DeepCopy() to make a deep copy of original object and modify this copy
	// Or create a copy manually for better performance

	woc := wfOperationCtx{
		wf:      wf.DeepCopyObject().(*wfv1.Workflow),
		updated: false,
		log: log.WithFields(log.Fields{
			"workflow":  wf.ObjectMeta.Name,
			"namespace": wf.ObjectMeta.Namespace,
		}),
		controller: wfc,
	}
	defer func() {
		if woc.updated {
			_, err := wfc.WorkflowClient.UpdateWorkflow(woc.wf)
			if err != nil {
				woc.log.Errorf("ERROR updating status: %v", err)
			} else {
				woc.log.Infof("UPDATED: %#v", woc.wf.Status)
			}
		}
	}()
	if woc.wf.Status.Nodes == nil {
		woc.wf.Status.Nodes = make(map[string]wfv1.NodeStatus)
		woc.updated = true
	}

	err := woc.executeTemplate(wf.Spec.Entrypoint, nil, wf.ObjectMeta.Name)
	if err != nil {
		woc.log.Errorf("%s error: %+v", wf.ObjectMeta.Name, err)
	}
}

func (woc *wfOperationCtx) executeTemplate(templateName string, args *wfv1.Arguments, nodeName string) error {
	woc.log.Infof("Evaluating node %s: %v, args: %#v", nodeName, templateName, args)
	nodeID := woc.wf.NodeID(nodeName)
	node, ok := woc.wf.Status.Nodes[nodeID]
	if ok && node.Completed() {
		woc.log.Infof("Node %s already completed", nodeName)
		return nil
	}
	tmpl := woc.wf.GetTemplate(templateName)
	if tmpl == nil {
		err := errors.Errorf(errors.CodeBadRequest, "Node %s error: template '%s' undefined", nodeName, templateName)
		woc.wf.Status.Nodes[nodeID] = wfv1.NodeStatus{ID: nodeID, Name: nodeName, Status: wfv1.NodeStatusError}
		woc.updated = true
		return err
	}

	switch tmpl.Type {
	case wfv1.TypeContainer:
		if !ok {
			// We have not yet created the pod
			status := wfv1.NodeStatusRunning
			err := woc.createWorkflowPod(nodeName, tmpl, args)
			if err != nil {
				// TODO: may need to query pod status if we hit already exists error
				status = wfv1.NodeStatusError
				return err
			}
			node = wfv1.NodeStatus{ID: nodeID, Name: nodeName, Status: status}
			woc.wf.Status.Nodes[nodeID] = node
			woc.log.Infof("Initialized container node %v", node)
			woc.updated = true
			return nil
		}
		return nil

	case wfv1.TypeWorkflow:
		if !ok {
			node = wfv1.NodeStatus{ID: nodeID, Name: nodeName, Status: wfv1.NodeStatusRunning}
			woc.log.Infof("Initialized workflow node %v", node)
			woc.wf.Status.Nodes[nodeID] = node
			woc.updated = true
		}
		for i, stepGroup := range tmpl.Steps {
			sgNodeName := fmt.Sprintf("%s[%d]", nodeName, i)
			err := woc.executeStepGroup(stepGroup, sgNodeName)
			if err != nil {
				node.Status = wfv1.NodeStatusError
				woc.wf.Status.Nodes[nodeID] = node
				woc.updated = true
				return err
			}
			sgNodeID := woc.wf.NodeID(sgNodeName)
			if !woc.wf.Status.Nodes[sgNodeID].Completed() {
				woc.log.Infof("Workflow step group node %v not yet completed", woc.wf.Status.Nodes[sgNodeID])
				return nil
			}
			if !woc.wf.Status.Nodes[sgNodeID].Successful() {
				woc.log.Infof("Workflow step group %v not successful", woc.wf.Status.Nodes[sgNodeID])
				node.Status = wfv1.NodeStatusFailed
				woc.wf.Status.Nodes[nodeID] = node
				woc.updated = true
				return nil
			}
		}
		node.Status = wfv1.NodeStatusSucceeded
		woc.wf.Status.Nodes[nodeID] = node
		woc.updated = true
		return nil

	default:
		woc.wf.Status.Nodes[nodeID] = wfv1.NodeStatus{ID: nodeID, Name: nodeName, Status: wfv1.NodeStatusError}
		woc.updated = true
		return errors.Errorf("Unknown type: %s", tmpl.Type)
	}
}

func (woc *wfOperationCtx) executeStepGroup(stepGroup map[string]wfv1.WorkflowStep, nodeName string) error {
	nodeID := woc.wf.NodeID(nodeName)
	node, ok := woc.wf.Status.Nodes[nodeID]
	if ok && node.Completed() {
		woc.log.Infof("Step group node %v already marked completed", node)
		return nil
	}
	if !ok {
		node = wfv1.NodeStatus{ID: nodeID, Name: nodeName, Status: "Running"}
		woc.wf.Status.Nodes[nodeID] = node
		woc.log.Infof("Initializing step group node %v", node)
		woc.updated = true
	}
	childNodeIDs := make([]string, 0)
	// First kick off all parallel steps in the group
	for stepName, step := range stepGroup {
		childNodeName := fmt.Sprintf("%s.%s", nodeName, stepName)
		childNodeIDs = append(childNodeIDs, woc.wf.NodeID(childNodeName))
		err := woc.executeTemplate(step.Template, &step.Arguments, childNodeName)
		if err != nil {
			node.Status = wfv1.NodeStatusError
			woc.wf.Status.Nodes[nodeID] = node
			woc.updated = true
			return err
		}
	}
	// Return if not all children completed
	for _, childNodeID := range childNodeIDs {
		if !woc.wf.Status.Nodes[childNodeID].Completed() {
			return nil
		}
	}
	// All children completed. Determine status
	for _, childNodeID := range childNodeIDs {
		if !woc.wf.Status.Nodes[childNodeID].Successful() {
			node.Status = wfv1.NodeStatusFailed
			woc.wf.Status.Nodes[nodeID] = node
			woc.updated = true
			woc.log.Infof("Step group node %s deemed failed due to failure of %s", nodeID, childNodeID)
			return nil
		}
	}
	node.Status = wfv1.NodeStatusSucceeded
	woc.wf.Status.Nodes[nodeID] = node
	woc.updated = true
	woc.log.Infof("Step group node %s successful", nodeID)
	return nil
}