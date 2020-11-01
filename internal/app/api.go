package app

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	protoV1 "github.com/golang/protobuf/proto"
	"github.com/mitchellh/mapstructure"
	"github.com/wailsapp/wails"
	"github.com/wailsapp/wails/cmd"
	"github.com/wailsapp/wails/lib/logger"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

const defaultWorkspaceKey = "wksp_default"

type api struct {
	runtime          *wails.Runtime
	logger           *logger.CustomLogger
	client           *client
	store            *store
	protofiles       *protoregistry.Files
	streamReq        chan proto.Message
	cancelMonitoring context.CancelFunc
	cancelInFlight   context.CancelFunc
	mu               sync.Mutex
	inFlight         bool
}

// WailsInit is the init fuction for the wails runtime
func (a *api) WailsInit(runtime *wails.Runtime) error {
	a.runtime = runtime
	a.logger = runtime.Log.New("API")

	// TODO get app data file path per os
	dbPath := filepath.Join(".", ".data")

	var err error
	a.store, err = newStore(dbPath)
	if err != nil {
		return fmt.Errorf("app: failed to create database: %v", err)
	}

	ready := "wails:ready"
	if wails.BuildMode == cmd.BuildModeBridge {
		fmt.Printf(" = %+v\n", ready)
		ready = "wails:loaded"
	}

	a.runtime.Events.On(ready, a.wailsReady)

	return nil
}

func (a *api) wailsReady(data ...interface{}) {
	opts, err := a.GetWorkspaceOptions()
	if err != nil {
		a.logger.Errorf("%v", err)
		return
	}
	if err := a.Connect(opts); err != nil {
		a.logger.Errorf("%v", err)
	}
}

// WailsShutdown is the shutdown function that is called when wails shuts down
func (a *api) WailsShutdown() {
	a.store.close()
	if a.cancelMonitoring != nil {
		a.cancelMonitoring()
	}
	if a.cancelInFlight != nil {
		a.cancelInFlight()
	}
	if a.client != nil {
		a.client.close()
	}
}

// GetWorkspaceOptions gets the workspace options from the store
func (a *api) GetWorkspaceOptions() (*options, error) {
	val, err := a.store.get([]byte(defaultWorkspaceKey))
	if err != nil {
		return nil, err
	}

	var opts *options
	dec := gob.NewDecoder(bytes.NewBuffer(val))
	err = dec.Decode(&opts)

	return opts, err
}

// Connect will attempt to connect a grpc server and parse any proto files
func (a *api) Connect(data interface{}) error {
	var opts options
	if err := mapstructure.Decode(data, &opts); err != nil {
		return err
	}

	if a.client != nil {
		if err := a.client.close(); err != nil {
			return fmt.Errorf("app: failed to close previous connection: %v", err)
		}
	}

	if a.cancelMonitoring != nil {
		a.cancelMonitoring()
	}

	a.client = &client{}
	if err := a.client.connect(opts, a); err != nil {
		return fmt.Errorf("app: failed to connect to server: %v", err)
	}

	a.runtime.Events.Emit(eventClientConnected, opts.Addr)

	ctx := context.Background()
	ctx, a.cancelMonitoring = context.WithCancel(ctx)
	go a.monitorStateChanges(ctx)

	go a.loadProtoFiles(opts)
	go a.setWorkspaceOptions(opts)

	return nil
}

func (a *api) loadProtoFiles(opts options) {
	a.runtime.Events.Emit(eventServicesSelectChanged)

	var err error
	if opts.Reflect {
		if a.client == nil {
			a.logger.Error("unable to load proto files via reflection: client is <nil>")
		}
		if a.protofiles, err = protoFilesFromReflectionAPI(a.client.conn, nil); err != nil {
			//TODO Emit error to frontend
			a.logger.Errorf("error getting proto files from reflection API: %v", err)
		}
	}
	if !opts.Reflect {
		// TODO: load protos from disk
	}

	a.emitServicesSelect()
}

func (a *api) emitServicesSelect() {
	if a.protofiles == nil {
		return
	}

	var ss servicesSelect
	a.protofiles.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		sds := fd.Services()
		for i := 0; i < sds.Len(); i++ {
			var s serviceSelect
			sd := sds.Get(i)
			s.FullName = string(sd.FullName())

			mds := sd.Methods()
			for j := 0; j < mds.Len(); j++ {
				md := mds.Get(j)
				fname := fmt.Sprintf("/%s/%s", sd.FullName(), md.Name())
				s.Methods = append(s.Methods, methodSelect{
					Name:     string(md.Name()),
					FullName: fname,
				})
			}
			sort.SliceStable(s.Methods, func(i, j int) bool {
				return s.Methods[i].Name < s.Methods[j].Name
			})
			ss = append(ss, s)
		}
		return true
	})

	if len(ss) == 0 {
		return
	}

	sort.SliceStable(ss, func(i, j int) bool {
		return ss[i].FullName < ss[j].FullName
	})

	a.runtime.Events.Emit(eventServicesSelectChanged, ss)
}

func (a *api) setWorkspaceOptions(opts options) {
	var val bytes.Buffer
	enc := gob.NewEncoder(&val)
	enc.Encode(opts)
	a.store.set([]byte(defaultWorkspaceKey), val.Bytes())
}

func (a *api) monitorStateChanges(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			// this will panic if we are waiting for a state change and the client (and it's connection)
			// get GC'd without this context being canceled
			a.logger.Errorf("panic monitoring state changes: %v", r)
		}
	}()
	for {
		if a.client == nil || a.client.conn == nil {
			continue
		}
		state := a.client.conn.GetState()
		a.runtime.Events.Emit(eventClientStateChanged, state.String())
		if ok := a.client.conn.WaitForStateChange(ctx, state); !ok {
			a.logger.Debug("ending monitoring of state changes")
			return
		}
	}
}

func (a *api) getMethodDesc(fullname string) (protoreflect.MethodDescriptor, error) {
	name := strings.Replace(fullname[1:], "/", ".", 1)
	desc, err := a.protofiles.FindDescriptorByName(protoreflect.FullName(name))
	if err != nil {
		return nil, fmt.Errorf("app: failed to find descriptor: %v", err)
	}

	methodDesc, ok := desc.(protoreflect.MethodDescriptor)
	if !ok {
		return nil, fmt.Errorf("app: descriptor was not a method: %T", desc)
	}

	return methodDesc, nil
}

// SelectMethod is called when the user selects a new method by the given name
func (a *api) SelectMethod(fullname string) error {
	methodDesc, err := a.getMethodDesc(fullname)
	if err != nil {
		return err
	}

	in := messageViewFromDesc(methodDesc.Input())
	a.runtime.Events.Emit(eventMethodInputChanged, in)

	return nil
}

func messageViewFromDesc(md protoreflect.MessageDescriptor) *messageDesc {
	var rtn messageDesc
	rtn.FullName = string(md.FullName())

	fds := md.Fields()
	rtn.Fields = fieldViewsFromDesc(fds, false)

	return &rtn
}

func fieldViewsFromDesc(fds protoreflect.FieldDescriptors, isOneof bool) []fieldDesc {
	var fields []fieldDesc

	seenOneof := make(map[protoreflect.Name]struct{})
	for i := 0; i < fds.Len(); i++ {
		fd := fds.Get(i)
		var fdesc fieldDesc
		fdesc.Name = string(fd.Name())
		fdesc.Kind = fd.Kind().String()
		fdesc.FullName = string(fd.FullName())

		// TODO(rogchap): check for IsList() instead and then also use IsMap()
		// to render maps differently rather than treating them as repeated messages
		fdesc.Repeated = fd.Cardinality() == protoreflect.Repeated

		if !isOneof {
			if oneof := fd.ContainingOneof(); oneof != nil {
				if _, ok := seenOneof[oneof.Name()]; ok {
					continue
				}
				fdesc.Name = string(oneof.Name())
				fdesc.Kind = "oneof"
				fdesc.Oneof = fieldViewsFromDesc(oneof.Fields(), true)

				seenOneof[oneof.Name()] = struct{}{}
				goto appendField
			}
		}

		if emd := fd.Enum(); emd != nil {
			evals := emd.Values()
			for i := 0; i < evals.Len(); i++ {
				eval := evals.Get(i)
				fdesc.Enum = append(fdesc.Enum, string(eval.Name()))
			}
		}

		if fmd := fd.Message(); fmd != nil {
			fdesc.Message = messageViewFromDesc(fmd)
		}

	appendField:
		fields = append(fields, fdesc)
	}
	return fields
}

func (a *api) Send(method string, rawJSON []byte) error {
	md, err := a.getMethodDesc(method)
	if err != nil {
		return err
	}

	req := dynamicpb.NewMessage(md.Input())
	if err := protojson.Unmarshal(rawJSON, req); err != nil {
		return err
	}

	if a.inFlight && md.IsStreamingClient() {
		a.streamReq <- req
		return nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.inFlight = true
	defer func() {
		a.inFlight = false
	}()

	ctx := context.Background()
	ctx, a.cancelInFlight = context.WithCancel(ctx)

	a.runtime.Events.Emit(eventRPCStarted, rpcStart{
		ClientStream: md.IsStreamingClient(),
		ServerStream: md.IsStreamingServer(),
	})

	if md.IsStreamingClient() && md.IsStreamingServer() {
		//TODO(rogchao) manage bidi requests
		return nil
	}

	if md.IsStreamingClient() {
		stream, err := a.client.invokeClientStream(ctx, method)
		if err != nil {
			return err
		}
		a.streamReq = make(chan proto.Message)
		a.streamReq <- req
		for r := range a.streamReq {
			if err := stream.SendMsg(r); err != nil {
				close(a.streamReq)
			}
		}
		stream.CloseAndReceive()

		return nil
	}

	if md.IsStreamingServer() {
		stream, err := a.client.invokeServerStream(ctx, method, req)
		if err != nil {
			return err
		}
		for {
			resp := dynamicpb.NewMessage(md.Output())
			if err := stream.RecvMsg(resp); err != nil {
				break
			}
		}

		return nil
	}

	resp := dynamicpb.NewMessage(md.Output())
	a.client.invoke(ctx, method, req, resp)
	return nil
}

// TagConn implements the stats.Handler interface
func (*api) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	// noop
	return ctx
}

// HandleConn implements the stats.Handler interface
func (*api) HandleConn(context.Context, stats.ConnStats) {
	// noop
}

// TagRPC implements the stats.Handler interface
func (*api) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	// noop
	return ctx
}

// HandleRPC implements the stats.Handler interface
func (a *api) HandleRPC(ctx context.Context, stat stats.RPCStats) {
	if internal := ctx.Value(ctxInternalKey{}); internal != nil {
		return
	}

	switch s := stat.(type) {
	case *stats.InPayload:
		txt, err := formatPayload(s.Payload)
		if err != nil {
			a.logger.Errorf("failed to marshal in payload to proto text: %v", err)
			return
		}
		a.runtime.Events.Emit(eventInPayloadReceived, txt)
	case *stats.End:
		stus := status.Convert(s.Error)
		var end rpcEnd
		end.StatusCode = int32(stus.Code())
		end.Status = stus.Code().String()
		end.Duration = s.EndTime.Sub(s.BeginTime).String()
		a.runtime.Events.Emit(eventRPCEnded, end)

	}
}

func formatPayload(payload interface{}) (string, error) {
	msg, ok := payload.(proto.Message)
	if !ok {
		// check to see if we are dealing with a APIv1 message
		msgV1, ok := payload.(protoV1.Message)
		if !ok {
			return "", fmt.Errorf("payload is not a proto message: %T", payload)
		}
		msg = protoV1.MessageV2(msgV1)
	}

	marshaler := prototext.MarshalOptions{
		Multiline: true,
		Indent:    "  ",
	}
	b, err := marshaler.Marshal(msg)
	if err != nil {
		return "", err
	}

	return string(b), nil
}

// Cancel will attempt to cancel the current inflight request
func (a *api) Cancel() {
	if a.cancelInFlight != nil {
		a.cancelInFlight()
	}
}
