package gitops

import "github.com/winezer0/processkit/processrunner"

func emitProcessEvent(observer func(OperationEvent), operation, engine string, result processrunner.Result) {
	if observer == nil || result.State == "" {
		return
	}
	snapshot := result.Snapshot
	process := &ProcessSnapshot{
		Name: snapshot.Name, State: snapshot.State, StopReason: string(snapshot.StopReason),
		ExitCode: snapshot.ExitCode, StartedAt: snapshot.StartedAt, FinishedAt: snapshot.FinishedAt,
		OutputBytes: snapshot.OutputBytes, Truncated: snapshot.Truncated,
	}
	emitProgress(observer, OperationEvent{Kind: EventProcess, Operation: operation, Engine: engine, Process: process})
}
