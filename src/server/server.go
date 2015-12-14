package server

import (
	. "common"
	"container/list"
	//"encoding/json"
	"net"
	"storage"
	"storage/memory"
	"strconv"
	"sync/atomic"
	"time"
	"utils/logger"
)

var ( //const replys, to avoid building it every time
	wakeupReply  = constructReply(NOOP, nil)
	nojobReply   = constructReply(NO_JOB, nil)
	timeoutReply = constructReply(WORK_FAIL, [][]byte{[]byte("job timeout")})
)

type Tuple struct {
	t0, t1, t2, t3, t4, t5 interface{}
}

type Event struct {
	tp            uint32
	args          *Tuple
	result        chan interface{}
	fromSessionId int64
	jobHandle     string
}

type Server struct {
	protoEvtCh     chan *Event
	startSessionId int64
	funcWorker     map[string]*JobWorkerMap
	worker         map[int64]*Worker
	client         map[int64]*Client
	workJobs       map[string]*Job
	funcTimeout    map[string]int
	jobStores      map[string]storage.JobQueue
}

func NewServer() *Server {
	return &Server{
		funcWorker:     make(map[string]*JobWorkerMap),
		protoEvtCh:     make(chan *Event, 100),
		worker:         make(map[int64]*Worker),
		client:         make(map[int64]*Client),
		workJobs:       make(map[string]*Job),
		jobStores:      make(map[string]storage.JobQueue),
		funcTimeout:    make(map[string]int),
		startSessionId: 1,
	}
}

func (server *Server) allocSessionId() int64 {
	return atomic.AddInt64(&server.startSessionId, 1)
}

func (server *Server) clearTimeoutJob() {

	now := time.Now().Unix()
	for k, j := range server.workJobs {
		if j.TimeoutSec > 0 {
			if (j.CreateAt.Unix() + int64(j.TimeoutSec)) > now {
				c, ok := server.client[j.CreateBy]
				if ok {
					c.Send(timeoutReply)
				}
				delete(server.workJobs, k)
			}
		}
	}

}

func (server *Server) Start(addr string) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Logger().E("%v", err)
	}

	logger.Logger().I("listening on %v", addr)
	go server.EvtLoop()

	for {
		conn, err := ln.Accept()
		if err != nil { // handle error
			continue
		}

		session := &Session{}
		go session.handleConnection(server, conn)
	}
}

func (server *Server) EvtLoop() {
	tick := time.NewTicker(2 * time.Second)
	for {
		select {
		case e := <-server.protoEvtCh:
			server.handleProtoEvt(e)
		case <-tick.C:
			logger.Logger().T("protoEvtCh:%v, worker:%v clientCount:%v",
				len(server.protoEvtCh), len(server.worker), len(server.client))
			server.clearTimeoutJob()
		}
	}
}

func (server *Server) addWorker(l *list.List, w *Worker) {
	for it := l.Front(); it != nil; it = it.Next() {
		if it.Value.(*Worker).SessionId == w.SessionId {
			logger.Logger().W("already add")
			return
		}
	}

	l.PushBack(w) //add to worker list
}

func (server *Server) getJobWorkPair(funcName string) *JobWorkerMap {
	jw, ok := server.funcWorker[funcName]
	if !ok { //create list
		jw = &JobWorkerMap{Workers: list.New()}
		server.funcWorker[funcName] = jw
	}

	return jw
}

func (server *Server) handleCanDo(funcName string, w *Worker, timeout int) {

	jw := server.getJobWorkPair(funcName)
	server.addWorker(jw.Workers, w)
	server.worker[w.SessionId] = w
	server.funcTimeout[funcName] = timeout
	w.canDo[funcName] = true

}

func (server *Server) addFuncJobStore(funcName string) storage.JobQueue {

	k, ok := server.jobStores[funcName]

	if ok {
		return k
	}

	queue := &memory.MemJobQueue{}
	queue.Initial(funcName)
	server.jobStores[funcName] = queue

	return queue
}

func (server *Server) removeCanDo(funcName string, sessionId int64) {

	if jw, ok := server.funcWorker[funcName]; ok {
		server.removeWorker(jw.Workers, sessionId)
	}

	delete(server.worker[sessionId].canDo, funcName)
}

func (server *Server) removeWorkerBySessionId(sessionId int64) {
	for _, jw := range server.funcWorker {
		server.removeWorker(jw.Workers, sessionId)
	}
	delete(server.worker, sessionId)
}

func (server *Server) removeWorker(l *list.List, sessionId int64) {
	for it := l.Front(); it != nil; it = it.Next() {
		if it.Value.(*Worker).SessionId == sessionId {
			logger.Logger().T("removeWorker sessionId %d", sessionId)
			l.Remove(it)
			return
		}
	}
}

func (server *Server) popJob(sessionId int64) *Job {

	for funcName, cando := range server.worker[sessionId].canDo {
		if !cando {
			continue
		}

		if queue, ok := server.jobStores[funcName]; ok {
			if queue.Length() == 0 {
				continue
			}

			jb := queue.PopJob()
			if jb != nil {
				return jb
			}
		}
	}

	return nil

}

func (server *Server) wakeupWorker(funcName string, w *Worker) bool {
	if queue, ok := server.jobStores[funcName]; ok {
		if queue.Length() > 0 {
			logger.Logger().T("wakeup sessionId", w.SessionId)
			w.Send(wakeupReply)
			return true
		}
	}
	return false
}

func (server *Server) handleSubmitJob(e *Event) {
	args := e.args
	c := args.t0.(*Client)

	server.client[c.SessionId] = c

	funcName := bytes2str(args.t1)

	timeout := 0
	v, ok := server.funcTimeout[funcName]
	if ok {
		timeout = v
	}

	j := &Job{Id: bytes2str(args.t2), Data: args.t3.([]byte),
		Handle: allocJobId(), CreateAt: time.Now(), CreateBy: c.SessionId,
		FuncName: funcName, Priority: PRIORITY_LOW, TimeoutSec: timeout}

	j.IsBackGround = isBackGround(e.tp)

	logger.Logger().T("add job %+v", j)

	j.Priority = cmd2Priority(e.tp)

	e.result <- j.Handle

	server.doAddJob(j)
}

func (server *Server) doAddJob(j *Job) {

	queue := server.addFuncJobStore(j.FuncName)
	j.ProcessBy = 0
	queue.PushJob(j)
	workers, ok := server.funcWorker[j.FuncName]
	if ok {
		for i, it := 0, workers.Workers.Front(); it != nil && i < 3; it = it.Next() {
			server.wakeupWorker(j.FuncName, it.Value.(*Worker))
			i++
		}
	}

}

func (sever *Server) checkAndRemoveJob(tp uint32, j *Job) {
	switch tp {
	case WORK_COMPLETE, WORK_EXCEPTION, WORK_FAIL:
		sever.removeJob(j)
	}
}

func (sever *Server) removeJob(j *Job) {
	delete(sever.workJobs, j.Handle)
}

func (server *Server) handleWorkReport(e *Event) {

	args := e.args
	slice := args.t0.([][]byte)
	jobhandle := bytes2str(slice[0])
	//sessionId := e.fromSessionId

	logger.Logger().T("%v job handle %v", CmdDescription(e.tp), jobhandle)

	j, ok := server.workJobs[jobhandle]
	if !ok {
		logger.Logger().W("job lost:%v  handle %v", CmdDescription(e.tp), jobhandle)
		return
	}

	if j.Handle != jobhandle {
		logger.Logger().E("job handle not match")
	}

	if WORK_STATUS == e.tp {
		j.Percent, _ = strconv.Atoi(string(slice[1]))
		j.Denominator, _ = strconv.Atoi(string(slice[2]))
	}

	if j.IsBackGround {
		return
	}

	c, ok := server.client[j.CreateBy]
	if !ok {
		logger.Logger().W("sessionId missing %v %v", j.Handle, j.CreateBy)
		return
	}

	reply := constructReply(e.tp, slice)
	c.Send(reply)
}

func (server *Server) handleCloseSession(e *Event) {
	sessionId := e.fromSessionId
	if w, ok := server.worker[sessionId]; ok {
		if sessionId != w.SessionId {
			logger.Logger().E("sessionId not match %d-%d, bug found", sessionId, w.SessionId)
		}
		logger.Logger().T("removeWorker sessionId", sessionId)
		server.removeWorkerBySessionId(w.SessionId)
	} else if c, ok := server.client[sessionId]; ok {
		logger.Logger().T("removeClient sessionId", sessionId)
		delete(server.client, c.SessionId)
	}
	e.result <- true
}

func (server *Server) handleCtrlEvt(e *Event) {

	switch e.tp {
	case ctrlCloseSession:
		server.handleCloseSession(e)
		return
	default:
		logger.Logger().W("%s, %d", CmdDescription(e.tp), e.tp)
	}

	return
}

func (server *Server) handleProtoEvt(e *Event) {
	args := e.args

	if e.tp >= ctrlCloseSession {
		server.handleCtrlEvt(e)
		return
	}

	switch e.tp {
	case CAN_DO:
		w := args.t0.(*Worker)
		funcName := args.t1.(string)
		timeout := 0
		server.handleCanDo(funcName, w, timeout)
		server.addFuncJobStore(funcName)
		break
	case CAN_DO_TIMEOUT:
		w := args.t0.(*Worker)
		funcName := args.t1.(string)
		timeout, err := strconv.Atoi(args.t2.(string))
		if err != nil {
			timeout = 0
			logger.Logger().W("timeout conv error, funcName %v", funcName)
		}
		server.handleCanDo(funcName, w, timeout)
		break
	case CANT_DO:
		sessionId := e.fromSessionId
		funcName := args.t0.(string)
		server.removeCanDo(funcName, sessionId)
		break
	case SET_CLIENT_ID:
		w := args.t0.(*Worker)
		w.workerId = args.t1.(string)
		break
	case GRAB_JOB, GRAB_JOB_UNIQ:

		sessionId := e.fromSessionId
		w, ok := server.worker[sessionId]
		if !ok {
			logger.Logger().W("unregister worker, sessionId %d", sessionId)
			break
		}

		w.status = wsRunning

		j := server.popJob(sessionId)
		if j != nil {
			j.ProcessAt = time.Now()
			j.ProcessBy = sessionId
			server.workJobs[j.Handle] = j
		} else { //no job
			w.status = wsPrepareForSleep
		}
		//send job back
		e.result <- j
		break
	case PRE_SLEEP:
		sessionId := e.fromSessionId
		w, ok := server.worker[sessionId]
		if !ok {
			logger.Logger().W("unregister worker, sessionId %d", sessionId)
			w = args.t0.(*Worker)
			server.worker[w.SessionId] = w
			break
		}
		w.status = wsSleep
		logger.Logger().T("worker sessionId %d sleep", sessionId)
		//check if there are any jobs for this worker
		for k, v := range w.canDo {
			if v && server.wakeupWorker(k, w) {
				break
			}
		}
		break
	case SUBMIT_JOB, SUBMIT_JOB_LOW_BG, SUBMIT_JOB_LOW:
		server.handleSubmitJob(e)
		break
	case WORK_DATA, WORK_WARNING, WORK_STATUS, WORK_COMPLETE,
		WORK_FAIL, WORK_EXCEPTION:
		server.handleWorkReport(e)
		break
	case RESET_ABILITIES:
		break
	default:
		logger.Logger().W("not support command:%s, %d", CmdDescription(e.tp), e.tp)
	}
}
