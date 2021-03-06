package execution_server

import (
	"context"
	"fmt"
	"io"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/buildbuddy-io/buildbuddy/server/environment"
	"github.com/buildbuddy-io/buildbuddy/server/interfaces"
	"github.com/buildbuddy-io/buildbuddy/server/remote_cache/digest"
	"github.com/buildbuddy-io/buildbuddy/server/remote_execution/operation"
	"github.com/buildbuddy-io/buildbuddy/server/util/perms"
	"github.com/buildbuddy-io/buildbuddy/server/util/status"
	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
	"google.golang.org/genproto/googleapis/longrunning"
	"google.golang.org/grpc/codes"

	repb "github.com/buildbuddy-io/buildbuddy/proto/remote_execution"
	durationpb "github.com/golang/protobuf/ptypes/duration"
	tspb "github.com/golang/protobuf/ptypes/timestamp"
)

const (
	// 7 days? Forever. This is the duration returned when no max duration
	// has been set in the config and no timeout was set in the client
	// request. It's basically the same as "no-timeout".
	infiniteDuration = time.Hour * 24 * 7
)

func getPlatformKey(platform *repb.Platform) string {
	props := make(map[string]string, len(platform.GetProperties()))
	for _, property := range platform.GetProperties() {
		props[property.GetName()] = property.GetValue()
	}
	return HashProperties(props)
}

func diffTimeProtos(startPb, endPb *tspb.Timestamp) time.Duration {
	start, _ := ptypes.Timestamp(startPb)
	end, _ := ptypes.Timestamp(endPb)
	return end.Sub(start)
}

func logActionResult(d *repb.Digest, md *repb.ExecutedActionMetadata) {
	qTime := diffTimeProtos(md.GetQueuedTimestamp(), md.GetWorkerStartTimestamp())
	workTime := diffTimeProtos(md.GetWorkerStartTimestamp(), md.GetWorkerCompletedTimestamp())
	fetchTime := diffTimeProtos(md.GetInputFetchStartTimestamp(), md.GetInputFetchCompletedTimestamp())
	execTime := diffTimeProtos(md.GetExecutionStartTimestamp(), md.GetExecutionCompletedTimestamp())
	uploadTime := diffTimeProtos(md.GetOutputUploadStartTimestamp(), md.GetOutputUploadCompletedTimestamp())
	log.Printf("%q completed action '%s/%d' [q: %02dms work: %02dms, fetch: %02dms, exec: %02dms, upload: %02dms]",
		md.GetWorker(), d.GetHash(), d.GetSizeBytes(), qTime.Milliseconds(), workTime.Milliseconds(),
		fetchTime.Milliseconds(), execTime.Milliseconds(), uploadTime.Milliseconds())
}

func extractStage(op *longrunning.Operation) repb.ExecutionStage_Value {
	md := &repb.ExecuteOperationMetadata{}
	if err := ptypes.UnmarshalAny(op.GetMetadata(), md); err != nil {
		return repb.ExecutionStage_UNKNOWN
	}
	return md.GetStage()
}

func extractActionResult(op *longrunning.Operation) *repb.ActionResult {
	er := &repb.ExecuteResponse{}
	if result := op.GetResult(); result != nil {
		if response, ok := result.(*longrunning.Operation_Response); ok {
			if err := ptypes.UnmarshalAny(response.Response, er); err == nil {
				return er.GetResult()
			}
		}
	}
	return nil
}

func HashProperties(props map[string]string) string {
	pairs := make([]string, 0, len(props))
	for k, v := range props {
		pairs = append(pairs, fmt.Sprintf("%s=%s", strings.ToLower(k), strings.ToLower(v)))
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "###")
}

type ExecutionServer struct {
	env   environment.Env
	cache interfaces.Cache
	exDB  interfaces.ExecutionDB
}

func NewExecutionServer(env environment.Env) (*ExecutionServer, error) {
	cache := env.GetCache()
	if cache == nil {
		return nil, fmt.Errorf("A cache is required to enable the RemoteExecutionServer")
	}
	exDB := env.GetExecutionDB()
	if exDB == nil {
		return nil, fmt.Errorf("An executionDB is required to enable the RemoteExecutionServer")
	}
	return &ExecutionServer{
		env:   env,
		cache: cache,
		exDB:  exDB,
	}, nil
}

func (s *ExecutionServer) readProtoFromCache(ctx context.Context, d *repb.Digest, msg proto.Message) error {
	ck, err := perms.UserPrefixCacheKey(ctx, s.env, d.GetHash())
	if err != nil {
		return err
	}

	data, err := s.cache.Get(ctx, ck)
	if err != nil {
		return err
	}
	return proto.Unmarshal(data, msg)
}

type finalizerFn func()

func parseTimeout(timeout *durationpb.Duration, maxDuration time.Duration) (time.Duration, error) {
	if timeout == nil {
		if maxDuration == 0 {
			return infiniteDuration, nil
		}
		return maxDuration, nil
	}
	requestDuration, err := ptypes.Duration(timeout)
	if err != nil {
		return 0, err
	}
	if maxDuration != 0 && requestDuration > maxDuration {
		return 0, status.FailedPreconditionErrorf("Specified timeout (%s) longer than server max (%s).", requestDuration, maxDuration)
	}
	return requestDuration, nil
}

// Execute an action remotely.
//
// In order to execute an action, the client must first upload all of the
// inputs, the
// [Command][build.bazel.remote.execution.v2.Command] to run, and the
// [Action][build.bazel.remote.execution.v2.Action] into the
// [ContentAddressableStorage][build.bazel.remote.execution.v2.ContentAddressableStorage].
// It then calls `Execute` with an `action_digest` referring to them. The
// server will run the action and eventually return the result.
//
// The input `Action`'s fields MUST meet the various canonicalization
// requirements specified in the documentation for their types so that it has
// the same digest as other logically equivalent `Action`s. The server MAY
// enforce the requirements and return errors if a non-canonical input is
// received. It MAY also proceed without verifying some or all of the
// requirements, such as for performance reasons. If the server does not
// verify the requirement, then it will treat the `Action` as distinct from
// another logically equivalent action if they hash differently.
//
// Returns a stream of
// [google.longrunning.Operation][google.longrunning.Operation] messages
// describing the resulting execution, with eventual `response`
// [ExecuteResponse][build.bazel.remote.execution.v2.ExecuteResponse]. The
// `metadata` on the operation is of type
// [ExecuteOperationMetadata][build.bazel.remote.execution.v2.ExecuteOperationMetadata].
//
// If the client remains connected after the first response is returned after
// the server, then updates are streamed as if the client had called
// [WaitExecution][build.bazel.remote.execution.v2.Execution.WaitExecution]
// until the execution completes or the request reaches an error. The
// operation can also be queried using [Operations
// API][google.longrunning.Operations.GetOperation].
//
// The server NEED NOT implement other methods or functionality of the
// Operations API.
//
// Errors discovered during creation of the `Operation` will be reported
// as gRPC Status errors, while errors that occurred while running the
// action will be reported in the `status` field of the `ExecuteResponse`. The
// server MUST NOT set the `error` field of the `Operation` proto.
// The possible errors include:
//
// * `INVALID_ARGUMENT`: One or more arguments are invalid.
// * `FAILED_PRECONDITION`: One or more errors occurred in setting up the
//   action requested, such as a missing input or command or no worker being
//   available. The client may be able to fix the errors and retry.
// * `RESOURCE_EXHAUSTED`: There is insufficient quota of some resource to run
//   the action.
// * `UNAVAILABLE`: Due to a transient condition, such as all workers being
//   occupied (and the server does not support a queue), the action could not
//   be started. The client should retry.
// * `INTERNAL`: An internal error occurred in the execution engine or the
//   worker.
// * `DEADLINE_EXCEEDED`: The execution timed out.
// * `CANCELLED`: The operation was cancelled by the client. This status is
//   only possible if the server implements the Operations API CancelOperation
//   method, and it was called for the current execution.
//
// In the case of a missing input or command, the server SHOULD additionally
// send a [PreconditionFailure][google.rpc.PreconditionFailure] error detail
// where, for each requested blob not present in the CAS, there is a
// `Violation` with a `type` of `MISSING` and a `subject` of
// `"blobs/{hash}/{size}"` indicating the digest of the missing blob.
func (s *ExecutionServer) Execute(req *repb.ExecuteRequest, stream repb.Execution_ExecuteServer) error {
	// The way this API is designed; clients can send a request and then
	// hang up, and check on responses using the WaitExecution API or
	// GetOperation (longrunning operation) API.
	//
	// The way we handle this is -- we open a connection to the worker which
	// remains open until the worker finishes or we timeout. Upon receiving
	// state updates from the worker, we write them to the DB, and send them
	// back to the calling client (bazel), if it remains connected.
	//
	// WaitExecution and GetOperation requests are handled by reading the
	// state from the DB.
	requestStartTimePb := ptypes.TimestampNow()
	action := &repb.Action{}
	if err := s.readProtoFromCache(stream.Context(), req.GetActionDigest(), action); err != nil {
		return status.FailedPreconditionErrorf("Error reading action: %s", err)
	}
	cmd := &repb.Command{}
	if err := s.readProtoFromCache(stream.Context(), action.GetCommandDigest(), cmd); err != nil {
		return status.FailedPreconditionErrorf("Error reading command: %s (action: %v)", err, action)
	}
	execClientConfig, err := s.env.GetExecutionClient(getPlatformKey(cmd.GetPlatform()))
	if err != nil {
		return status.FailedPreconditionErrorf("No worker enabled for platform %v: %s", cmd.GetPlatform(), err)
	}
	exClient := execClientConfig.GetExecutionClient()
	duration, err := parseTimeout(action.Timeout, execClientConfig.GetMaxDuration())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(stream.Context(), duration)
	defer cancel()

	actionDigestName := digest.GetResourceName(req.GetActionDigest())
	// writeProgressFn writes progress to our stream (if open) else the DB.
	writeProgressFn := func(stage repb.ExecutionStage_Value, op *longrunning.Operation) error {
		err := stream.Send(op)
		if err != nil {
			if err := s.exDB.InsertOrUpdateExecution(ctx, actionDigestName, stage, op); err != nil {
				return err
			}
		}
		return nil
	}
	// completeLastOperationFn rewrites the operation to include the correct "queued" timestamp.
	completeLastOperationFn := func(op *longrunning.Operation) error {
		actionResult := extractActionResult(op)
		md := actionResult.GetExecutionMetadata()
		md.QueuedTimestamp = requestStartTimePb
		// logActionResult(req.GetActionDigest(), md)
		_, newOp, err := operation.Assemble(repb.ExecutionStage_COMPLETED, req.GetActionDigest(), codes.OK, actionResult)
		op = newOp
		return err
	}

	var finalizer finalizerFn = func() {
		stage := repb.ExecutionStage_COMPLETED
		if op, err := operation.AssembleFailed(stage, req.GetActionDigest(), codes.Internal); err == nil {
			log.Printf("Failed action %s (returning code INTERNAL)", req.GetActionDigest())
			writeProgressFn(stage, op)
		}
	}
	// Defer a call to the failsafe finalizer if it's not been set to nil.
	// This will mark the operation as COMPLETE with an INTERNAL error code
	// so that clients who call WaitExecution do not wait forever.
	defer func() {
		if finalizer != nil {
			finalizer()
		}
	}()

	// Synchronous RPC mode
	if execClientConfig.DisableStreaming() {
		syncResponse, err := exClient.SyncExecute(ctx, req)
		if err != nil {
			return err
		}
		for _, op := range syncResponse.GetOperations() {
			stage := extractStage(op)
			if stage == repb.ExecutionStage_COMPLETED {
				if err := completeLastOperationFn(op); err != nil {
					return err
				}
				finalizer = nil
			}
			if err := writeProgressFn(stage, op); err != nil {
				return err
			}
		}
		return nil
	}
	// End Synchronous RPC mode

	workerStream, err := exClient.Execute(ctx, req)
	if err != nil {
		return err
	}
	defer workerStream.CloseSend()

	for {
		op, readErr := workerStream.Recv()
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			log.Printf("Worker encountered err: %s", readErr)
			return err
		}
		stage := extractStage(op)
		if stage == repb.ExecutionStage_COMPLETED {
			if err := completeLastOperationFn(op); err != nil {
				return err
			}
			finalizer = nil
		}
		if err := writeProgressFn(stage, op); err != nil {
			return err
		}
	}
	return nil
}

// Wait for an execution operation to complete. When the client initially
// makes the request, the server immediately responds with the current status
// of the execution. The server will leave the request stream open until the
// operation completes, and then respond with the completed operation. The
// server MAY choose to stream additional updates as execution progresses,
// such as to provide an update as to the state of the execution.
func (s *ExecutionServer) WaitExecution(req *repb.WaitExecutionRequest, stream repb.Execution_WaitExecutionServer) error {
	for {
		execution, err := s.exDB.ReadExecution(stream.Context(), req.GetName())
		if err != nil {
			return err
		}
		op := &longrunning.Operation{}
		if err := proto.Unmarshal(execution.SerializedOperation, op); err != nil {
			return err
		}
		err = stream.Send(op)
		if err == io.EOF {
			break // If the caller hung-up, bail out.
		}
		if err != nil {
			return err // If some other err happened; bail out.
		}
		stage := extractStage(op)
		if stage == repb.ExecutionStage_COMPLETED {
			break // If the operation is complete, bail out.
		}

		// Sleep for a little while before checking the DB again.
		time.Sleep(1 * time.Second)
	}
	return nil
}

func (s *ExecutionServer) SyncExecute(ctx context.Context, req *repb.ExecuteRequest) (*repb.SyncExecuteResponse, error) {
	return nil, status.UnimplementedErrorf("Not implemented")
}
