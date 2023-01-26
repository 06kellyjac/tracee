package ebpf

import (
	gocontext "context"
	"encoding/binary"
	"fmt"

	"github.com/aquasecurity/tracee/pkg/events"
	"github.com/aquasecurity/tracee/pkg/logger"
	"github.com/aquasecurity/tracee/types/trace"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

//
// User chooses through config/cmdline how to capture pcap files:
//
// - single file
// - per process
// - per container
// - per command
//
// and might have more than 1 way enabled simultaneously.
//

const (
	familyIpv4 int = 1 << iota
	familyIpv6
)

func (t *Tracee) processNetCaptureEvents(ctx gocontext.Context) {
	var errChanList []<-chan error

	// source pipeline stage (re-used from regular pipeline)
	eventsChan, errChan := t.decodeEvents(ctx, t.netCapChannel)
	errChanList = append(errChanList, errChan)

	// process events stage (network capture only)
	errChan = t.processNetCapEvents(ctx, eventsChan)
	errChanList = append(errChanList, errChan)

	// pipeline started, wait for completion.
	t.WaitForPipeline(errChanList...)
}

func (t *Tracee) processNetCapEvents(ctx gocontext.Context, in <-chan *trace.Event) <-chan error {
	errc := make(chan error, 1)

	go func() {
		defer close(errc)

		for {
			select {
			case event := <-in:
				t.processNetCapEvent(event)
				t.stats.NetCapCount.Increment()

			case lost := <-t.lostNetCapChannel:
				if lost > 0 {
					// https://github.com/aquasecurity/libbpfgo/issues/122
					t.stats.LostNtCapCount.Increment(lost)
					logger.Warn(fmt.Sprintf("lost %d network capture events", lost))
				}

			case <-ctx.Done():
				return
			}
		}
	}()

	return errc
}

// processNetCapEvent processes network packets meant to be captured.
//
// TODO: usually networking parsing functions are big, still, this might need
// some refactoring to make it smaller (code reuse might not be a key for the
// refactor).
func (t *Tracee) processNetCapEvent(event *trace.Event) {
	eventId := events.ID(event.EventID)

	switch eventId {
	case events.NetPacketCapture:
		var ok bool
		var payload []byte
		var layerType gopacket.LayerType

		// sanity checks

		payloadArg := events.GetArg(event, "payload")
		if payloadArg == nil {
			logger.Debug("network capture: no payload packet")
			return
		}
		if payload, ok = payloadArg.Value.([]byte); !ok {
			logger.Debug("network capture: non []byte argument")
			return
		}
		payloadSize := len(payload)
		if payloadSize < 1 {
			logger.Debug("network capture: empty payload")
			return
		}

		// event retval encodes layer 3 protocol type

		if event.ReturnValue&familyIpv4 == familyIpv4 {
			layerType = layers.LayerTypeIPv4
		} else if event.ReturnValue&familyIpv6 == familyIpv6 {
			layerType = layers.LayerTypeIPv6
		} else {
			logger.Debug("unsupported layer3 protocol")
		}

		// parse packet

		packet := gopacket.NewPacket(
			payload[4:payloadSize], // base event argument is: |sizeof|[]byte|
			layerType,
			gopacket.Default,
		)
		if packet == nil {
			logger.Debug("could not parse packet")
			return
		}

		// amount of bytes the TCP header has based on data offset field

		tcpDoff := func(l4 gopacket.TransportLayer) uint32 {
			var doff uint32
			if v, ok := l4.(*layers.TCP); ok {
				doff = 20                    // TCP header default length is 20 bytes
				if v.DataOffset > uint8(5) { // unless doff is set, then...
					doff = uint32(v.DataOffset) * 4 // doff * 32bit words == tcp header length
				}
			}
			return doff
		}

		// NOTES:
		//
		// 1) Fake Layer 2:
		//
		// Tracee captures L3 packets only, but pcap needs a L2 header, as it
		// mixes IPv4 and IPv6 packets in the same pcap file.
		//
		// The easiest link type is "Null", which emulates a BSD loopback
		// encapsulation (4-byte field differentiating IPv4 and IPv6 packets).
		//
		// So, from now on, instead of having the initial 32-bit as the "sizeof"
		// (the event argument), it will become this "fake L2 header" as if it
		// were the BSD loopback encapsulation header.
		//
		// 2) Fake IP header length Field:
		//
		// Tcpdump, when reading the generated pcap files, will complain about
		// missing packet payload if the IP header says one length and the
		// actual data in the payload is smaller (what happens when tracee
		// pcap-snaplen option is not set to max). The code bellow changes IP
		// length field to the length of the captured data.
		//

		captureLength := t.config.Capture.Net.CaptureLength // after last known header

		// parse packet
		layer3 := packet.NetworkLayer()
		layer4 := packet.TransportLayer()

		ipHeaderLength := uint32(0)     // IP header length is dynamic
		icmpHeaderLength := uint32(8)   // ICMP header length is 8 bytes
		icmpv6HeaderLength := uint32(4) // ICMPv6 header length is 4 bytes
		udpHeaderLength := uint32(8)    // UDP header length is 8 bytes
		tcpHeaderLength := uint32(0)    // TCP header length is dynamic

		// will calculate L4 protocol headers length value
		ipHeaderLengthValue := uint32(0)
		udpHeaderLengthValue := uint32(0)

		switch v := layer3.(type) {

		case (*layers.IPv4):
			// Fake L2 header: IPv4 (BSD encap header spec)
			binary.BigEndian.PutUint32(payload, 2) // set value 2 to first 4 bytes (uint32)

			// IP header depends on IHL flag (default: 5 * 4 = 20 bytes)
			ipHeaderLength += uint32(v.IHL) * 4
			ipHeaderLengthValue += ipHeaderLength

			switch v.Protocol {
			case layers.IPProtocolICMPv4:
				// ICMP
				ipHeaderLengthValue += icmpHeaderLength
			case layers.IPProtocolUDP:
				// UDP
				udpHeaderLengthValue += udpHeaderLength
				ipHeaderLengthValue += udpHeaderLength
			case layers.IPProtocolTCP:
				// TCP
				tcpHeaderLength = tcpDoff(layer4)
				ipHeaderLengthValue += tcpHeaderLength
			}

			// add capture length (length to capture after last known proto header)
			ipHeaderLengthValue += captureLength
			udpHeaderLengthValue += captureLength

			// capture length is bigger than the pkt payload: no need for mangling
			if ipHeaderLengthValue != uint32(len(payload[4:])) {
				break
			} // else: mangle the packet (below) due to capture length

			// sanity check for max uint16 size in IP header length field
			if ipHeaderLengthValue >= (1 << 16) {
				ipHeaderLengthValue = (1 << 16) - 1
			}

			// change IPv4 total length field for the correct (new) packet size
			binary.BigEndian.PutUint16(payload[6:], uint16(ipHeaderLengthValue))
			// no flags, frag offset OR checksum changes (tcpdump does not complain)

			switch v.Protocol {
			// TCP does not have a length field (uses checksum to verify)
			// no checksum recalculation (tcpdump does not complain)
			case layers.IPProtocolUDP:
				// NOTE: tcpdump might complain when parsing UDP packets that
				//       are meant for a specific L7 protocol, like DNS, for
				//       example, if their port is the protocol port and user
				//       is only capturing "headers". That happens because it
				//       tries to parse the DNS header and, if it does not
				//       exist, it causes an error. To avoid that, one can run
				//       tcpdump -q -r ./file.pcap, so it does not try to parse
				//       upper layers in detail. That is the reason why the
				//       default pcap snaplen is 96b.
				//
				// change UDP header length field for the correct (new) size
				binary.BigEndian.PutUint16(
					payload[4+ipHeaderLength+4:],
					uint16(udpHeaderLengthValue),
				)
			}

		case (*layers.IPv6):
			// Fake L2 header: IPv6 (BSD encap header spec)
			binary.BigEndian.PutUint32(payload, 28) // set value 28 to first 4 bytes (uint32)

			ipHeaderLength = uint32(40) // IPv6 does not have an IHL field
			ipHeaderLengthValue += ipHeaderLength

			switch v.NextHeader {
			case layers.IPProtocolICMPv6:
				// ICMPv6
				ipHeaderLengthValue += icmpv6HeaderLength
			case layers.IPProtocolUDP:
				// UDP
				udpHeaderLengthValue += udpHeaderLength
				ipHeaderLengthValue += udpHeaderLength
			case layers.IPProtocolTCP:
				// TCP
				tcpHeaderLength = tcpDoff(layer4)
				ipHeaderLengthValue += tcpHeaderLength
			}

			// add capture length (length to capture after last known proto header)
			ipHeaderLengthValue += captureLength
			udpHeaderLengthValue += captureLength

			// capture length is bigger than the pkt payload: no need for mangling
			if ipHeaderLengthValue != uint32(len(payload[4:])) {
				break
			} // else: mangle the packet (below) due to capture length

			// sanity check for max uint16 size in IP header length field
			if ipHeaderLengthValue >= (1 << 16) {
				ipHeaderLengthValue = (1 << 16) - 1
			}

			// change IPv6 payload length field for the correct (new) packet size
			binary.BigEndian.PutUint16(payload[12:], uint16(ipHeaderLengthValue))
			// no flags, frag offset OR checksum changes (tcpdump does not complain)

			switch v.NextHeader {
			// TCP does not have a length field (uses checksum to verify)
			// no checksum recalculation (tcpdump does not complain)
			case layers.IPProtocolUDP:
				// NOTE: same as IPv4 note
				// change UDP header length field for the correct (new) size
				binary.BigEndian.PutUint16(
					payload[4+ipHeaderLength+4:],
					uint16(udpHeaderLengthValue),
				)
			}

		default:
			return
		}

		// This might be too much, but keep it here for now

		// logger.Debug(
		// 	"capturing network",
		// 	"command", event.ProcessName,
		// 	"srcIP", srcIP,
		// 	"dstIP", dstIP,
		// )

		// capture the packet to all enabled pcap files

		err := t.netCapturePcap.Write(event, payload)
		if err != nil {
			logger.Error("could not write pcap data", "err", err)
		}

	default:
		logger.Debug("network capture: wrong net capture event type")
	}
}