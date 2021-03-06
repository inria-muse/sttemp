package servicetraceroute

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"
)

//Lifetime of logs inside the map before the expiration
// const LogTTL int64 = 600 //seconds?

//Interval for Offsets
//[start, end)
type OffsetInterval struct {
	start int //Start of the interval (included)
	end   int //End of the interval (not included)
}

//Configuration of ServiceTraceroute Manager
type ServiceTracerouteManagerConfiguration struct {
	Interface string                 //Interface used to listen/send packets
	BorderIPs []net.IP               //Router where traceTCP will stop (if flag is set to True)
	Services  []ServiceConfiguration //Services to trace
	IPVersion string                 //IP version

	Sniffer     bool //True for auto sniffing from library
	DNSResolver bool //True for using DNS Service detection
	Sender      bool //True to send automatically packets

	DestinationMultipleProbing bool //To start multiple probing towards the same destination
	PortMultipleProbing        bool //To start multiple probing towards the same destination port

	MaxConsecutiveMissingHops int //Maximum number of missing hops (= no replyies to all probes) are required before stopping traceroute

	DNSResolverConfFile string //Filename containing configuration of the dns resolver

	//Debug
	StartTraceroutes bool //To only obtain the IPs of the server that would have been analysed
	Verbose          bool
	StartAnalysis    bool //Specify if ServiceTraceroute can start new traceroutes or not
}

//Log of ServiceTraceroute which will be stored in the map
type ServiceTracerouteLog struct {
	Report        ServiceTracerouteJson          //Final report, if traceTCP completed
	Configuration ServiceTracerouteConfiguration //Configuration used for ServiceTraceroute

	IsRunning  bool  //Flag to specify if it is running or not
	StartedAt  int64 //When ServiceTraceroute started
	FinishedAt int64 //When ServiceTraceroute finished (if not, then it is negative)
}

//Main struct for the manager of multiple ServiceTraceroute
type ServiceTracerouteManager struct {
	RunningServiceTraceroutes map[string]*ServiceTraceroute //Contains all running experiments
	AvailableOffsets          []OffsetInterval              //ID intervals available for new ServiceTraceroutes

	//Configuration
	Configuration ServiceTracerouteManagerConfiguration //Configuration of ServiceTraceroute

	//Input Channels (captured packets)
	TCPChan  chan gopacket.Packet //Input channel for TCP packets to be forwarded to ServiceTraceroute
	UDPChan  chan gopacket.Packet //Input channel for UDP packets to be forwarded to ServiceTraceroute
	ICMPChan chan gopacket.Packet //Input channel for ICMP packets to be forwarded to ServiceTraceroute
	DNSChan  chan gopacket.Packet //Input channel for DNS packets to be forwarded to ServiceTraceroute

	//Output Channels
	OutPacketChan chan []byte                //packets to be transmitted
	OutChan       chan string                //data to be printed/stored
	OutResultChan chan ServiceTracerouteJson //results to show

	//Channel to stop ServiceTracerouteManager
	StopChan chan bool //To stop the manager

	//DNS resolver
	DNS *DNSResolver

	//Packet Listeners
	Listeners *Listeners

	//Packet Sender
	Sender *Sender

	//Local IPs
	LocalIPv4 net.IP //Local IPv4 of the machine running this library
	LocalIPv6 net.IP //Local IPv6 of the machine running this library

	//Mutex
	offsetMutex        *sync.Mutex //Mutex for offset array
	runningTracesMutex *sync.Mutex //Mutex for running ServiceTraceroute array
	logsMapMutex       *sync.Mutex //Mutex for map with logs

	//Results
	LogsMap    map[string]ServiceTracerouteLog //Contains the running experiments and the results of those finished in the last 3 minutes
	LogsMapTTL int64                           //time to live for data in logs map when the experiment is finished
}

//NewServiceTracerouteManager initialize the manager of multiple ServiceTraceroute experiments
//iface string: is the name of the interface. It must be set
//ipVersion string: is the version of the IP layer ('4' or '6'). Use the const V4 or V6
//borderRouters []net.IP: are the IPs of the border routers, where ServiceTraceroute will stop. It can be nil if not used
//return error: nil if no error happened during the initialization
func (tm *ServiceTracerouteManager) NewServiceTracerouteManager(iface string, ipVersion string, parallelProbesPerDestination bool, parallelProbesPerDstPort bool, startSniffer bool, startSender bool, startDNSResolver bool, startTraceroutes bool, interTraceTime int, maxConsecutiveMissingHops int, borderRouters []net.IP, outChan chan string, outResultsChan chan ServiceTracerouteJson) error {
	//Initial configuration
	tm.Configuration.Sniffer = startSniffer
	tm.Configuration.DNSResolver = startDNSResolver
	tm.Configuration.Sender = startSender
	tm.Configuration.Interface = iface
	tm.Configuration.IPVersion = ipVersion
	tm.Configuration.StartTraceroutes = startTraceroutes
	tm.Configuration.Verbose = false
	tm.Configuration.StartAnalysis = true

	tm.Configuration.DNSResolverConfFile = "input.conf"
	tm.Configuration.DestinationMultipleProbing = parallelProbesPerDestination
	tm.Configuration.PortMultipleProbing = parallelProbesPerDstPort

	tm.Configuration.MaxConsecutiveMissingHops = maxConsecutiveMissingHops

	tm.DNSChan = make(chan gopacket.Packet, 100)
	tm.TCPChan = make(chan gopacket.Packet, 100)
	tm.UDPChan = make(chan gopacket.Packet, 100)
	tm.ICMPChan = make(chan gopacket.Packet, 100)

	tm.OutChan = outChan
	tm.OutResultChan = outResultsChan

	tm.Listeners = new(Listeners)
	tm.Listeners.NewListeners(iface, outChan)

	//Start DNS module
	if tm.Configuration.DNSResolver {
		if tm.Configuration.Verbose {
			tm.OutChan <- fmt.Sprintf("Starting DNS resolver")
		}
		tm.Listeners.StartDNS(tm.DNSChan)
		tm.StartDNSResolver(tm.DNSChan)
	}
	//Start Sniffers
	if tm.Configuration.Sniffer {
		if tm.Configuration.Verbose {
			tm.OutChan <- fmt.Sprintf("Starting listeners on the interface")
		}
		tm.Listeners.StartTCP(tm.TCPChan)
		tm.Listeners.StartUDP(tm.UDPChan)
		tm.Listeners.StartICMP(tm.ICMPChan)
	}
	//Start transmitter
	if tm.Configuration.Sender {
		if tm.Configuration.Verbose {
			tm.OutChan <- fmt.Sprintf("Starting sender module")
		}
		tm.Sender = new(Sender)
		tm.Sender.NewSender(tm.Configuration.Interface, outChan)
		go tm.Sender.Run()
		tm.OutPacketChan = tm.Sender.SendQ
	} else {
		tm.OutPacketChan = make(chan []byte, 1000)
	}

	//Init
	tm.offsetMutex = &sync.Mutex{}
	tm.runningTracesMutex = &sync.Mutex{}
	tm.logsMapMutex = &sync.Mutex{}

	tm.RunningServiceTraceroutes = make(map[string]*ServiceTraceroute)
	tm.AvailableOffsets = make([]OffsetInterval, 1)
	tm.LogsMap = make(map[string]ServiceTracerouteLog)
	tm.LogsMapTTL = int64(interTraceTime)

	//Add interval long all possible IDs
	tm.AvailableOffsets[0] = OffsetInterval{
		start: 0,
		end:   math.MaxUint16,
	}

	//Get local IPs
	err := tm.SetLocalIPs()

	if err != nil {
		return err
	}

	//Set borderRouters
	tm.Configuration.BorderIPs = borderRouters

	return nil
}

//Convert the application flows 5-tuple into a key for identifying the traceroutes
func (tm *ServiceTracerouteManager) GetMapKey(protocol string, remoteIp net.IP, remotePort int, localPort int) string {
	if !tm.Configuration.DestinationMultipleProbing {
		return fmt.Sprintf("%s-%s", protocol, remoteIp.String())
	}
	if !tm.Configuration.PortMultipleProbing {
		return fmt.Sprintf("%s-%s-%d", protocol, remoteIp.String(), remotePort)
	}
	return fmt.Sprintf("%s-%s-%d-%d", protocol, remoteIp.String(), remotePort, localPort)
}

//Start the multiplexer for input packets. It forwards the input packets to the correct traceroute
func (tm *ServiceTracerouteManager) Run() {
	//Multiplexing of data between the running ServiceTraceroutes and external process
	for {
		select {
		case <-tm.StopChan:
			return

		//For TCP and UDP the logic is the same:
		//1 - Get a new packet
		//2 - Check if the packet is associated to a specific running traceroute
		//2.1 - If yes, send the packet to the traceroute and go to step 1
		//3 - If not, check if the packet can be associated with a traceroute waiting for the application flow (missing local port)
		//3.1 - If yes, redirect the packet to the traceroute, change the flow ID of the traceroute and go to step 1
		//4 - If there are no running or waiting traceroutes, check if the remote IP of the packet is inside an application flow to be studied (service)
		//4.1 - If yes, try to start a new traceroute (it depends on the checks done before starting the traceroute) and then go to step 1
		//5 - If not, drop the packet and go to step 1
		case tcpPacket := <-tm.TCPChan:
			ip1, port1, ip2, port2, err := tm.GetFlowIDFromTCPPacket(&tcpPacket)

			if err != nil {
				//ERROR: skip packet
				if tm.Configuration.Verbose {
					tm.OutChan <- err.Error()
				}
				continue
			}

			st := tm.GetTracerouteFromFlowID(ip1, port1, ip2, port2)

			//If no traceroute have the target flow id
			//Assign it to one with same destination and port
			if st == nil {
				st = tm.AssignFlowIDToTraceroute(ip1, port1, ip2, port2)
			}

			//Redirect packet
			if st != nil {
				st.SniffChannel <- &tcpPacket
				continue
			}

			if !tm.Configuration.DNSResolver {
				continue
			}

			var services []string
			var ipresolutions []string
			dstIp := ip1
			dstPort := port1
			srcPort := port2
			//No traceroute active with this flow ID
			//Try to resolve IP
			if res, err := tm.DNS.ResolveIP(ip1); err == nil {
				services = res.Names
				ipresolutions = res.IPResolutions
				dstIp = ip1
				dstPort = port1
				srcPort = port2
			} else if res, err := tm.DNS.ResolveIP(ip2); err == nil {
				services = res.Names
				ipresolutions = res.IPResolutions
				dstIp = ip2
				dstPort = port2
				srcPort = port1
			} else {
				//no resolver detected
				continue
			}
			tm.StartServiceTraceroute(Tcp, services, ipresolutions, dstIp, dstPort, srcPort)

		case udpPacket := <-tm.UDPChan:
			ip1, port1, ip2, port2, err := tm.GetFlowIDFromUDPPacket(&udpPacket)

			if err != nil {
				//ERROR: skip packet
				if tm.Configuration.Verbose {
					tm.OutChan <- err.Error()
				}
				continue
			}

			st := tm.GetTracerouteFromFlowID(ip1, port1, ip2, port2)

			//If no traceroute have the target flow id
			//Assign it to one with same destination and port
			if st == nil {
				st = tm.AssignFlowIDToTraceroute(ip1, port1, ip2, port2)
			}

			//Redirect packet
			if st != nil {
				st.SniffChannel <- &udpPacket
				continue
			}

			if !tm.Configuration.DNSResolver {
				continue
			}

			var services []string
			var ipresolutions []string
			dstIp := ip1
			dstPort := port1
			srcPort := port2
			//No traceroute active with this flow ID
			//Try to resolve IP
			if res, err := tm.DNS.ResolveIP(ip1); err == nil {
				services = res.Names
				ipresolutions = res.IPResolutions
				dstIp = ip1
				dstPort = port1
				srcPort = port2
			} else if res, err := tm.DNS.ResolveIP(ip2); err == nil {
				services = res.Names
				ipresolutions = res.IPResolutions
				dstIp = ip2
				dstPort = port2
				srcPort = port1
			} else {
				//no resolver detected
				continue
			}
			tm.StartServiceTraceroute(Udp, services, ipresolutions, dstIp, dstPort, srcPort)

		//For ICMP, obtain the flow id of the dropped packet
		//If no traceroutes are associated to the flow id, drop the packet
		case icmpPacket := <-tm.ICMPChan:
			dstIp, dstPort, srcIp, srcPort, err := tm.GetFlowIDFromICMPPacket(&icmpPacket)

			if err != nil {
				//ERROR: skip packet
				if tm.Configuration.Verbose {
					tm.OutChan <- err.Error()
				}
				continue
			}

			st := tm.GetTracerouteFromFlowID(dstIp, dstPort, srcIp, srcPort)

			if st == nil || st.Configuration.RemoteIP.String() != dstIp.String() {
				//ERROR: skip packet
				continue
			}

			st.SniffChannel <- &icmpPacket
		}
	}
}

//Stop the multiplexer (Run() function)
func (tm *ServiceTracerouteManager) Stop() {
	tm.StopChan <- true
}

//Start a traceceroute to a specific service if the given service is in the list of services to be analyzed
func (tm *ServiceTracerouteManager) StartServiceTraceroute(transportProtocol string, services []string, ipresolutions []string, dstIp net.IP, dstPort int, localPort int) {
	//Check if there is one service matching the new application flow
	for _, confService := range tm.Configuration.Services {
		for index, service := range services {
			//If matches, then start
			if strings.ToLower(confService.Service) == strings.ToLower(service) {
				//Start new traceroute
				go tm.StartTraceroute(
					transportProtocol,
					dstIp,
					dstPort,
					localPort,
					service,
					ipresolutions[index],
					confService.Distance,
					confService.Iterations,
					confService.Timeout,
					confService.FlowTimeout,
					confService.InterProbeTime,
					confService.InterIterationTime,
					confService.ProbingAlgorithm,
					confService.StopOnBorderRouters,
					confService.StartWithEmptyPacket,
					tm.Configuration.MaxConsecutiveMissingHops,
				)
				return
			}
		}
	}
}

//Open a new traceroute
//It must run on a thread, otherwise it locks the thread until the end
//If there is no space (i.e. no available offset spot), return error
//At the end of the run it will return the report through the outResultChan
//The new traceroute can start only if:
// - StartAnalysis is true (default)
// - There aren't any other traceroute for the same application flow
// - The results are expired in the log (depends by the time)
// - The IP versions are correct
func (tm *ServiceTracerouteManager) StartTraceroute(transportProtocol string, remoteIP net.IP, remotePort int, localPort int, service string, ipresolution string, maxDistance int, numberIterations int, timeout int, flowTimeout int, interProbeTime int, interIterationTime int, probingAlgorithm string, stopWithBorderRouters bool, startWithEmptyPacket bool, maxConsecutiveMissingHops int) error {
	tm.ClearLogsMap()

	//Check if the flag to start new traceroutes is True
	if !tm.Configuration.StartAnalysis {
		return errors.New("Start analysis set to False")
	}

	//check that there aren't any other traceroutes for the same application flow
	if tm.CheckExistanceServiceTracerouteExperiment(transportProtocol, remoteIP, remotePort, localPort) {
		return errors.New("ServiceTraceroute to " + remoteIP.String() + " is already running")
	}

	//check if the traceroute is in the log
	if _, err := tm.GetLog(transportProtocol, remoteIP.String(), remotePort, localPort); err == nil {
		return errors.New(fmt.Sprintf("ServiceTraceroute to %s finished within %d seconds", remoteIP.String(), tm.LogsMapTTL))
	}

	//Check that there is enough space
	size := maxDistance * numberIterations
	interval, err := tm.UseInterval(size)

	//If it was not possible to get the interval, return error
	if err != nil {
		return err
	}

	borderIPs := tm.Configuration.BorderIPs

	//If the ServiceTraceroute must not stop with border routers, just set the array to nil
	if !stopWithBorderRouters {
		borderIPs = nil
	}

	ipversion := V6
	if remoteIP.To4() != nil {
		ipversion = V4
	}

	//If the requested IP version is defined and it does not match with the IPVersion of the flow
	//Don't trace
	if tm.Configuration.IPVersion != "" && tm.Configuration.IPVersion != ipversion {
		return errors.New("Not the correct IP version")
	}

	//Generate wanted configuration
	config := ServiceTracerouteConfiguration{
		TransportProtocol:         transportProtocol,
		IDOffset:                  uint16(interval.start),
		BorderIPs:                 borderIPs,
		Service:                   service,
		IPResolution:              ipresolution,
		Distance:                  maxDistance,
		Interface:                 tm.Configuration.Interface,
		RemoteIP:                  remoteIP,
		RemotePort:                remotePort,
		LocalPort:                 localPort,
		InterIterationTime:        interIterationTime,
		InterProbeTime:            interProbeTime,
		IPVersion:                 ipversion,
		Iterations:                numberIterations,
		LocalIPv4:                 tm.LocalIPv4,
		LocalIPv6:                 tm.LocalIPv6,
		Timeout:                   timeout,
		ProbingAlgorithm:          strings.ToLower(probingAlgorithm),
		MaxConsecutiveMissingHops: maxConsecutiveMissingHops,
		StartWithEmptyPacket:      startWithEmptyPacket,
		FlowTimeout:               flowTimeout,
		StartTraceroutes:          tm.Configuration.StartTraceroutes,
	}

	log := ServiceTracerouteLog{
		Configuration: config,
		IsRunning:     true,
		FinishedAt:    -1,
		StartedAt:     time.Now().UnixNano(),
	}

	//Start  ServiceTraceroute on a new thread
	st := new(ServiceTraceroute)
	st.NewConfiguredServiceTraceroute(config)

	//Set output channel
	st.SetOutPacketsChan(tm.OutPacketChan)

	//Set 'stdout'
	st.SetStdOutChan(tm.OutChan)

	//Check if there are no servicetraceroute to the same destination
	//If no servicetraceroute, then store servicetraceroute as running experiment
	exists := tm.CheckAndAddServiceTracerouteExperiment(st)

	if exists {
		//Free the used interval
		tm.FreeInterval(interval)
		return errors.New("ServiceTraceroute to " + remoteIP.String() + " is already running")
	}

	tm.UpdateLogsMap(log)

	//Run ServiceTraceroute
	report := st.Run()

	tm.OutResultChan <- report

	log.Report = report
	log.FinishedAt = time.Now().UnixNano()
	log.IsRunning = false

	tm.UpdateLogsMap(log)

	//Finished, remove st from running experiments
	tm.RemoveServiceTracerouteExperiment(st)

	//Free the used interval
	tm.FreeInterval(interval)

	return nil
}

//###### MUTEX REQUIRED #######

//Find the interval which fits for the given interval size
//Remove spot from the available spots in AvailableOffsets
func (tm *ServiceTracerouteManager) UseInterval(size int) (OffsetInterval, error) {
	tm.offsetMutex.Lock()

	index := -1

	for i, interval := range tm.AvailableOffsets {
		if (interval.end - interval.start) >= size {
			index = i
			break
		}
	}

	//If no intervals are found
	if index < 0 {
		return OffsetInterval{}, errors.New("No offsets available for the required size")
	}

	interval := OffsetInterval{
		start: tm.AvailableOffsets[index].start,
		end:   tm.AvailableOffsets[index].start + size,
	}

	//Check if the interval must be removed or just resized
	if (tm.AvailableOffsets[index].end - tm.AvailableOffsets[index].start) == size {
		tm.AvailableOffsets = append(tm.AvailableOffsets[:index], tm.AvailableOffsets[:index+1]...)
	} else {
		tm.AvailableOffsets[index].start += size
	}

	tm.offsetMutex.Unlock()

	return interval, nil
}

//Free an used interval and put it into AvailableOffsets
func (tm *ServiceTracerouteManager) FreeInterval(offsetInterval OffsetInterval) {
	tm.offsetMutex.Lock()

	for _, interval := range tm.AvailableOffsets {
		//The interval to be added was before 'interval'
		if offsetInterval.end == interval.start {
			interval.start = offsetInterval.end
			offsetInterval = interval
		}
		if offsetInterval.start == interval.end {
			interval.end = offsetInterval.start
			offsetInterval = interval
		}
	}

	tm.offsetMutex.Unlock()
}

//Check if there is an already running traceroute
//However, if it says that there are no traceTCP to the remoteIP, it may happen  that
//a new experiment may be added immediately after it.
func (tm *ServiceTracerouteManager) CheckExistanceServiceTracerouteExperiment(protocol string, remoteIp net.IP, remotePort int, localPort int) bool {
	exists := false

	key := tm.GetMapKey(protocol, remoteIp, remotePort, localPort)
	tm.runningTracesMutex.Lock()

	if _, ok := tm.RunningServiceTraceroutes[key]; ok {
		exists = true
	}

	tm.runningTracesMutex.Unlock()

	return exists
}

//Check if there is an already running experiments
//If not, it adds the given ServiceTraceroute into the list of running experiments
func (tm *ServiceTracerouteManager) CheckAndAddServiceTracerouteExperiment(st *ServiceTraceroute) bool {
	exists := false

	key := tm.GetMapKey(st.Configuration.TransportProtocol, st.Configuration.RemoteIP, st.Configuration.RemotePort, st.Configuration.LocalPort)
	tm.runningTracesMutex.Lock()

	if _, ok := tm.RunningServiceTraceroutes[key]; ok {
		exists = true
	}

	if !exists {
		tm.RunningServiceTraceroutes[key] = st
	}

	tm.runningTracesMutex.Unlock()
	return exists
}

//Change the ID (key) identifying a traceroute
func (tm *ServiceTracerouteManager) SwitchTracerouteKeys(key string, oldkey string) {
	if _, ok := tm.RunningServiceTraceroutes[oldkey]; ok {
		st := tm.RunningServiceTraceroutes[oldkey]
		delete(tm.RunningServiceTraceroutes, oldkey)
		tm.RunningServiceTraceroutes[key] = st
	}
}

//Remove the input st from the array of running experiments
func (tm *ServiceTracerouteManager) RemoveServiceTracerouteExperiment(st *ServiceTraceroute) {
	tm.runningTracesMutex.Lock()

	key := tm.GetMapKey(st.Configuration.TransportProtocol, st.Configuration.RemoteIP, st.Configuration.RemotePort, st.Configuration.LocalPort)

	if _, ok := tm.RunningServiceTraceroutes[key]; ok {
		delete(tm.RunningServiceTraceroutes, key)
	}

	tm.runningTracesMutex.Unlock()
}

//Return ServiceTraceroute which contains the input IP ID
func (tm *ServiceTracerouteManager) GetTracerouteFromIPID(id uint16) *ServiceTraceroute {
	tm.runningTracesMutex.Lock()

	var st *ServiceTraceroute

	for _, runningServiceTraceroute := range tm.RunningServiceTraceroutes {
		size := uint16(runningServiceTraceroute.GetDistance() * runningServiceTraceroute.GetIterations())

		if id >= runningServiceTraceroute.GetIDOffset() && id < (runningServiceTraceroute.GetIDOffset()+size) {
			st = runningServiceTraceroute
			break
		}
	}

	tm.runningTracesMutex.Unlock()

	return st
}

//GetServiceTracerouteExperimentFromFlowID return the ServiceTraceroute where remoteIP and remotePort matches one of 2 pairs given as input (where one is local end host and the other is the remote one)
func (tm *ServiceTracerouteManager) GetTracerouteFromFlowID(ip1 net.IP, port1 int, ip2 net.IP, port2 int) *ServiceTraceroute {
	tm.runningTracesMutex.Lock()

	var st *ServiceTraceroute
	st = nil

	for _, runningServiceTraceroute := range tm.RunningServiceTraceroutes {
		//Check if flows IDs corresponds (checking both directions)
		if runningServiceTraceroute.Configuration.RemoteIP.String() == ip1.String() && runningServiceTraceroute.Configuration.RemotePort == port1 &&
			runningServiceTraceroute.Configuration.LocalIPv4.String() == ip2.String() && runningServiceTraceroute.Configuration.LocalPort == port2 {
			st = runningServiceTraceroute
			break
		} else if runningServiceTraceroute.Configuration.RemoteIP.String() == ip2.String() && runningServiceTraceroute.Configuration.RemotePort == port2 &&
			runningServiceTraceroute.Configuration.LocalIPv4.String() == ip1.String() && runningServiceTraceroute.Configuration.LocalPort == port1 {
			st = runningServiceTraceroute
			break
		}
	}

	tm.runningTracesMutex.Unlock()

	return st
}

//At the beginning a Traceroute may not have the complete application flow ID
//This function fixes the flow ID of the traceroute when it is incomplete
func (tm *ServiceTracerouteManager) AssignFlowIDToTraceroute(ip1 net.IP, port1 int, ip2 net.IP, port2 int) *ServiceTraceroute {
	tm.runningTracesMutex.Lock()

	var st *ServiceTraceroute
	st = nil
	modified := false

	for _, runningServiceTraceroute := range tm.RunningServiceTraceroutes {
		oldkey := tm.GetMapKey(runningServiceTraceroute.Configuration.TransportProtocol, runningServiceTraceroute.Configuration.RemoteIP, runningServiceTraceroute.Configuration.RemotePort, runningServiceTraceroute.Configuration.LocalPort)
		//Check if flows IDs corresponds (checking both directions)
		if runningServiceTraceroute.Configuration.RemoteIP.String() == ip1.String() && runningServiceTraceroute.Configuration.RemotePort == port1 && (runningServiceTraceroute.Configuration.LocalPort == 0 || port2 == runningServiceTraceroute.Configuration.LocalPort) {
			runningServiceTraceroute.Configuration.LocalIPv4 = ip2
			runningServiceTraceroute.Configuration.LocalPort = port2
			modified = true
			st = runningServiceTraceroute
		} else if runningServiceTraceroute.Configuration.RemoteIP.String() == ip2.String() && runningServiceTraceroute.Configuration.RemotePort == port2 && (runningServiceTraceroute.Configuration.LocalPort == 0 || port1 == runningServiceTraceroute.Configuration.LocalPort) {
			runningServiceTraceroute.Configuration.LocalIPv4 = ip1
			runningServiceTraceroute.Configuration.LocalPort = port1
			modified = true
			st = runningServiceTraceroute
		}
		key := tm.GetMapKey(runningServiceTraceroute.Configuration.TransportProtocol, runningServiceTraceroute.Configuration.RemoteIP, runningServiceTraceroute.Configuration.RemotePort, runningServiceTraceroute.Configuration.LocalPort)

		if modified {
			tm.logsMapMutex.Lock()
			tm.SwitchLogsKey(key, oldkey)
			tm.logsMapMutex.Unlock()
			tm.SwitchTracerouteKeys(key, oldkey)
			break
		}
	}

	tm.runningTracesMutex.Unlock()

	return st
}

//Return the number of running ServiceTraceroute
func (tm *ServiceTracerouteManager) GetNumberOfRunningServiceTraceroute() int {
	tm.runningTracesMutex.Lock()

	numberRunningExps := len(tm.RunningServiceTraceroutes)

	tm.runningTracesMutex.Unlock()

	return numberRunningExps
}

//Return the log of a traceroute for a specific application flow
func (tm *ServiceTracerouteManager) GetLog(protocol string, remoteIp string, remotePort int, localPort int) (ServiceTracerouteLog, error) {
	var log ServiceTracerouteLog = ServiceTracerouteLog{}
	var err error = nil

	tm.ClearLogsMap()

	tm.logsMapMutex.Lock()

	key := tm.GetMapKey(protocol, net.ParseIP(remoteIp), remotePort, localPort)

	if _, ok := tm.LogsMap[key]; ok {
		log = tm.LogsMap[key]
	} else {
		err = errors.New("Log not found")
	}

	tm.logsMapMutex.Unlock()

	return log, err
}

//Remove old logs and add/update the input log
func (tm *ServiceTracerouteManager) UpdateLogsMap(log ServiceTracerouteLog) error {
	tm.ClearLogsMap()

	var err error = nil
	tm.logsMapMutex.Lock()

	key := tm.GetMapKey(log.Configuration.TransportProtocol, log.Configuration.RemoteIP, log.Configuration.RemotePort, log.Configuration.LocalPort)
	tm.LogsMap[key] = log

	tm.logsMapMutex.Unlock()
	return err
}

//Remove a given log from the map
func (tm *ServiceTracerouteManager) RemoveLogsMap(log ServiceTracerouteLog) error {
	var err error = nil

	key := tm.GetMapKey(log.Configuration.TransportProtocol, log.Configuration.RemoteIP, log.Configuration.RemotePort, log.Configuration.LocalPort)
	if _, ok := tm.LogsMap[key]; ok {
		delete(tm.LogsMap, key)
	} else {
		err = errors.New("Log not found")
	}

	return err
}

//Change the key corresponding to a specific log
func (tm *ServiceTracerouteManager) SwitchLogsKey(key string, oldkey string) error {
	var err error = nil

	if _, ok := tm.LogsMap[oldkey]; ok {
		log := tm.LogsMap[oldkey]
		delete(tm.LogsMap, oldkey)
		tm.LogsMap[key] = log
	} else {
		err = errors.New("Log not found")
	}

	return err
}

//Remove all expired logs
func (tm *ServiceTracerouteManager) ClearLogsMap() {
	tm.logsMapMutex.Lock()

	for _, v := range tm.LogsMap {
		now := time.Now().UnixNano()

		if v.FinishedAt <= 0 || v.IsRunning {
			continue
		}

		if ((now-v.FinishedAt)/int64(time.Second)) > tm.LogsMapTTL && tm.LogsMapTTL >= 0 {
			tm.RemoveLogsMap(v)
		}
	}

	tm.logsMapMutex.Unlock()
}

//###### END MUTEX  #######

//###### PACKET PARSING  #######

//Get the Flow ID from a TCP packet
func (tm *ServiceTracerouteManager) GetFlowIDFromTCPPacket(tcpPacket *gopacket.Packet) (net.IP, int, net.IP, int, error) {
	var ip1 net.IP
	var ip2 net.IP
	var port1 int
	var port2 int
	var err error

	if ip4Layer := (*tcpPacket).Layer(layers.LayerTypeIPv4); ip4Layer != nil {
		ip1 = ip4Layer.(*layers.IPv4).SrcIP
		ip2 = ip4Layer.(*layers.IPv4).DstIP
	} else if ip6Layer := (*tcpPacket).Layer(layers.LayerTypeIPv6); ip6Layer != nil {
		ip1 = ip6Layer.(*layers.IPv4).SrcIP
		ip2 = ip6Layer.(*layers.IPv4).DstIP
	} else {
		err = errors.New("No IPv4 Packet")
	}

	if tcpLayer := (*tcpPacket).Layer(layers.LayerTypeTCP); tcpLayer != nil {
		port1 = tm.ConvertPort(tcpLayer.(*layers.TCP).SrcPort.String())
		port2 = tm.ConvertPort(tcpLayer.(*layers.TCP).DstPort.String())
	} else {
		err = errors.New("No TCP Packet")
	}

	return ip1, port1, ip2, port2, err
}

//Get the Flow Id from an UDP packet
func (tm *ServiceTracerouteManager) GetFlowIDFromUDPPacket(udpPacket *gopacket.Packet) (net.IP, int, net.IP, int, error) {
	var ip1 net.IP
	var ip2 net.IP
	var port1 int
	var port2 int
	var err error

	if ip4Layer := (*udpPacket).Layer(layers.LayerTypeIPv4); ip4Layer != nil {
		ip1 = ip4Layer.(*layers.IPv4).SrcIP
		ip2 = ip4Layer.(*layers.IPv4).DstIP
	} else if ip6Layer := (*udpPacket).Layer(layers.LayerTypeIPv6); ip6Layer != nil {
		ip1 = ip6Layer.(*layers.IPv6).SrcIP
		ip2 = ip6Layer.(*layers.IPv6).DstIP
	} else {
		err = errors.New("No IPv4 Packet")
	}

	if udpLayer := (*udpPacket).Layer(layers.LayerTypeUDP); udpLayer != nil {
		port1 = tm.ConvertPort(udpLayer.(*layers.UDP).SrcPort.String())
		port2 = tm.ConvertPort(udpLayer.(*layers.UDP).DstPort.String())
	} else {
		err = errors.New("No TCP Packet")
	}

	return ip1, port1, ip2, port2, err
}

//Return the IPID contained in the ICMP payload
func (tm *ServiceTracerouteManager) GetIPIDFromICMPPacket(icmpPacket *gopacket.Packet) (uint16, error) {
	if icmp4Layer := (*icmpPacket).Layer(layers.LayerTypeICMPv4); icmp4Layer != nil {
		icmp, _ := icmp4Layer.(*layers.ICMPv4)

		if icmp.TypeCode.Type() == layers.ICMPv4TypeTimeExceeded {
			payload := make([]byte, len(icmp.LayerPayload()))
			copy(payload, icmp.LayerPayload())

			var id uint16 = binary.BigEndian.Uint16(payload[4:6])

			return id, nil
		}
	}
	return 0, errors.New("Not an ICMPv4 Packet")
}

//Return the final destination of traceTCP probe from the payload of ICMP
func (tm *ServiceTracerouteManager) GetFlowIDFromICMPPacket(icmpPacket *gopacket.Packet) (net.IP, int, net.IP, int, error) {
	if icmp4Layer := (*icmpPacket).Layer(layers.LayerTypeICMPv4); icmp4Layer != nil {
		icmp, _ := icmp4Layer.(*layers.ICMPv4)

		if icmp.TypeCode.Type() == layers.ICMPv4TypeTimeExceeded {
			dstIp, srcIp, protocol, HL, err := tm.DecodeICMPIP(icmp.LayerPayload())
			dstPort := 0
			srcPort := 0
			if err != nil {
				return dstIp, dstPort, srcIp, srcPort, err
			}

			if protocol == Tcp {
				dstPort, srcPort, err = tm.DecodeICMPTCP(icmp.LayerPayload(), HL)
			} else if protocol == Udp {
				dstPort, srcPort, err = tm.DecodeICMPUDP(icmp.LayerPayload(), HL)
			}

			return dstIp, dstPort, srcIp, srcPort, err
		}
	}
	return nil, 0, nil, 0, errors.New("Not an ICMPv4 TTLExceeded Packet")
}

//Decode the ICMP payload to return the IP addresses, the protocol and header length of the dropped packet
func (tm *ServiceTracerouteManager) DecodeICMPIP(payload []byte) (net.IP, net.IP, string, int, error) {
	dstIp := make(net.IP, 4)
	srcIP := make(net.IP, 4)

	tmp := binary.BigEndian.Uint32(payload[12:16])
	binary.BigEndian.PutUint32(srcIP, tmp)
	tmp = binary.BigEndian.Uint32(payload[16:20])
	binary.BigEndian.PutUint32(dstIp, tmp)

	HL := binary.BigEndian.Uint16(payload[0:2])
	HL &= 0x0F00
	HL >>= 8
	HL *= 4

	protocol := binary.BigEndian.Uint16(payload[8:10])
	protocol &= 0x00FF

	if protocol == 6 {
		return dstIp, srcIP, Tcp, int(HL), nil
	} else if protocol == 17 {
		return dstIp, srcIP, Udp, int(HL), nil
	}

	return dstIp, srcIP, "unknown", int(HL), errors.New("Protocol not implemented")
}

//Return source and destination port contained in the ICMP payload (TCP packet)
func (tm *ServiceTracerouteManager) DecodeICMPTCP(payload []byte, HL int) (int, int, error) {

	srcPort := binary.BigEndian.Uint16(payload[HL : HL+2])
	dstPort := binary.BigEndian.Uint16(payload[HL+2 : HL+4])

	return int(dstPort), int(srcPort), nil
}

//Return source and destination port contained in the ICMP payload (UDP packet)
func (tm *ServiceTracerouteManager) DecodeICMPUDP(payload []byte, HL int) (int, int, error) {

	srcPort := binary.BigEndian.Uint16(payload[HL : HL+2])
	dstPort := binary.BigEndian.Uint16(payload[HL+2 : HL+4])

	return int(dstPort), int(srcPort), nil
}

//###### END PACKET PARSING  #######

//###### GET & SET  #######

//Set the local IPs taking the from the interface used by traceTCPmanager
func (tm *ServiceTracerouteManager) SetLocalIPs() error {
	devices, err := pcap.FindAllDevs()
	if err != nil {
		return err
	}

	validAddress := false

	for _, devIface := range devices {
		if devIface.Name == tm.Configuration.Interface {
			netIface, err := net.InterfaceByName(tm.Configuration.Interface)
			if err != nil {
				return err
			}
			addrs, _ := netIface.Addrs()
			for _, addr := range addrs {
				switch v := addr.(type) {
				case *net.IPNet:
					if v.IP.IsGlobalUnicast() && v.IP.To4() != nil {
						tm.LocalIPv4 = (*v).IP
						if tm.Configuration.IPVersion == V4 {
							validAddress = true
						}
					} else if v.IP.IsGlobalUnicast() && v.IP.To16() != nil {
						tm.LocalIPv6 = (*v).IP
						if tm.Configuration.IPVersion == V6 {
							tm.OutChan <- fmt.Sprintf("Local V6 %s", v.IP.String())
							validAddress = true
						}
					}
				}
			}
		}
	}

	if !validAddress {
		return errors.New("No valid IP for interface")
	}
	return nil
}

//Set border routers
func (tm *ServiceTracerouteManager) SetBorderRouters(borderIPs []net.IP) {
	tm.Configuration.BorderIPs = borderIPs
}

//Add one or an array of border routers
func (tm *ServiceTracerouteManager) AddBorderRouters(borderIPs ...net.IP) {
	tm.Configuration.BorderIPs = append(tm.Configuration.BorderIPs, borderIPs...)
}

//Set the services to start automatically traceroute
func (tm *ServiceTracerouteManager) SetServices(services []ServiceConfiguration) {
	tm.Configuration.Services = services
}

//Add a service to the set of services for the automatic traceroutes
func (tm *ServiceTracerouteManager) AddService(service ServiceConfiguration) {
	if tm.Configuration.DNSResolver {
		tm.DNS.UpdateService(service)
		for i, _ := range tm.Configuration.Services {
			if tm.Configuration.Services[i].Service == service.Service {
				return
			}
		}
		tm.Configuration.Services = append(tm.Configuration.Services, service)
	}
}

//Remove one service from the set of services for the automatic traceroutes
func (tm *ServiceTracerouteManager) RemoveService(service ServiceConfiguration) {
	if tm.Configuration.DNSResolver {
		index := -1
		for i, _ := range tm.Configuration.Services {
			if tm.Configuration.Services[i].Service == service.Service {
				index = i
				break
			}
		}
		if index < 0 {
			return
		}
		tm.Configuration.Services = append(tm.Configuration.Services[:index], tm.Configuration.Services[index+1:]...)
	}
}

//Load border routers from a file
func (tm *ServiceTracerouteManager) LoadBorderRouters(filename string) error {
	file, err := os.Open(filename)

	if err != nil {
		return err
	}

	defer file.Close()

	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		tm.AddBorderRouters(net.ParseIP(scanner.Text()))
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	return nil
}

//Return the list of border routers
func (tm *ServiceTracerouteManager) GetBorderRouters() []net.IP {
	return tm.Configuration.BorderIPs
}

//Set the stdout channel
func (tm *ServiceTracerouteManager) SetOutChan(outchan chan string) {
	tm.OutChan = outchan
}

//Return the used stdout channel
func (tm *ServiceTracerouteManager) GetOutChan() chan string {
	return tm.OutChan
}

//Set the channel used for the transmission of packets
func (tm *ServiceTracerouteManager) SetOutPktsChan(outPktsChan chan []byte) {
	tm.OutPacketChan = outPktsChan
}

//Get the channel used for the transmission of packets
func (tm *ServiceTracerouteManager) GetOutPktsChan() chan []byte {
	return tm.OutPacketChan
}

//Set the channel used for the sniffing ICMP packets
func (tm *ServiceTracerouteManager) SetICMPInChan(icmpChan chan gopacket.Packet) {
	tm.ICMPChan = icmpChan
}

//Set the channel used for the sniffing TCP packets
func (tm *ServiceTracerouteManager) SetTCPInChan(tcpChan chan gopacket.Packet) {
	tm.TCPChan = tcpChan
}

//Set the channel used for the sniffing UDP packets
func (tm *ServiceTracerouteManager) SetUDPInChan(udpChan chan gopacket.Packet) {
	tm.UDPChan = udpChan
}

//Get the channel used for the sniffing ICMP packets
func (tm *ServiceTracerouteManager) GetICMPInChan() chan gopacket.Packet {
	return tm.ICMPChan
}

//Get the channel used for the sniffing TCP packets
func (tm *ServiceTracerouteManager) GetTCPInChan() chan gopacket.Packet {
	return tm.TCPChan
}

//Get the channel used for the sniffing UDP packets
func (tm *ServiceTracerouteManager) GetUDPInChan() chan gopacket.Packet {
	return tm.UDPChan
}

//Convert a port from string to int (in some cases it may contain some text)
func (tm *ServiceTracerouteManager) ConvertPort(port string) int {
	if !strings.Contains(port, "(") {
		p, _ := strconv.Atoi(port)
		return p
	}
	p, _ := strconv.Atoi(port[:strings.Index(port, "(")])
	return p
}

//Start the DNS resolution using an external channel
func (tm *ServiceTracerouteManager) StartDNSResolver(dnsChan chan gopacket.Packet) error {
	if tm.DNS != nil {
		tm.OutChan <- "Error while starting dns resolver"
		return errors.New("DNS Resolver already enabled.")
	}
	tm.DNSChan = dnsChan
	tm.DNS = new(DNSResolver)
	tm.DNS.NewDNSResolver(tm.Configuration.DNSResolverConfFile, tm.DNSChan)
	tm.Configuration.DNSResolver = true
	go tm.DNS.Run()
	return nil
}

//Stop the DNS resolution
func (tm *ServiceTracerouteManager) StopDNSResolver() {
	if tm.DNS == nil {
		return
	}
	tm.DNS.Stop()
	tm.DNS = nil
}

//Set verbose flag
func (tm *ServiceTracerouteManager) SetVerbose(verbose bool) {
	tm.Configuration.Verbose = verbose
}

//Set the flag to start new traceroute. Used if the goal is to stop new traceroutes for a while
func (tm *ServiceTracerouteManager) SetStartNewTraceroutes(newTraceroutes bool) {
	tm.Configuration.StartAnalysis = newTraceroutes
}
