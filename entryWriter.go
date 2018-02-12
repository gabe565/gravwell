/*************************************************************************
 * Copyright 2017 Gravwell, Inc. All rights reserved.
 * Contact: <legal@gravwell.io>
 *
 * This software may be modified and distributed under the terms of the
 * BSD 2-clause license. See the LICENSE file for details.
 **************************************************************************/

package ingest

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/gravwell/ingest/entry"
)

const (
	ACK_SIZE int = 12 //ackmagic + entrySendID
	//READ_ENTRY_HEADER_SIZE should be 46 bytes
	//34 + 4 + 4 + 8 (magic, data len, entry ID)
	READ_ENTRY_HEADER_SIZE int = entry.ENTRY_HEADER_SIZE + 12
	//TODO: We should make this configurable by configuration
	MAX_ENTRY_SIZE              int           = 128 * 1024 * 1024
	WRITE_BUFFER_SIZE           int           = 4 * 1024 * 1024
	MAX_WRITE_ERROR             int           = 4
	NEW_ENTRY_MAGIC             uint32        = 0xC7C95ACB
	FORCE_ACK_MAGIC             uint32        = 0x1ADF7350
	CONFIRM_ENTRY_MAGIC         uint32        = 0xF6E0307E
	BUFFERED_ACK_READER_SIZE    int           = ACK_SIZE * MAX_UNCONFIRMED_COUNT
	CLOSING_SERVICE_ACK_TIMEOUT time.Duration = time.Second

	//MAX_UNCONFIRMED_COUNT MUST be > MIN_UNCONFIRMED_COUNT
	MIN_UNCONFIRMED_COUNT int = 64
	MAX_UNCONFIRMED_COUNT int = 1024 * 4
)

type entrySendID uint64

type EntryWriter struct {
	conn       net.Conn
	bIO        *bufio.Writer
	bAckReader *bufio.Reader
	errCount   uint32
	mtx        *sync.Mutex
	ecb        entryConfBuffer
	hot        bool
	buff       []byte
	id         entrySendID
	ackTimeout time.Duration
}

func NewEntryWriter(conn net.Conn) (*EntryWriter, error) {
	var bRdr *bufio.Reader
	var bWtr *bufio.Writer
	if MIN_UNCONFIRMED_COUNT >= MAX_UNCONFIRMED_COUNT {
		return nil, errors.New("MAX_UNCONFIRMED_COUNT must be >= MIN_UNCONFIRMED_COUNT")
	}
	ecb, err := newEntryConfirmationBuffer(MAX_UNCONFIRMED_COUNT)
	if err != nil {
		return nil, err
	}

	//acks are pretty small so this can be smaller
	bRdr = bufio.NewReaderSize(conn, BUFFERED_ACK_READER_SIZE)
	bWtr = bufio.NewWriterSize(conn, WRITE_BUFFER_SIZE)

	buff := make([]byte, READ_ENTRY_HEADER_SIZE)
	return &EntryWriter{
		conn:       conn,
		bIO:        bWtr,
		bAckReader: bRdr,
		mtx:        &sync.Mutex{},
		ecb:        ecb,
		hot:        true,
		buff:       buff,
		id:         1,
		ackTimeout: CLOSING_SERVICE_ACK_TIMEOUT,
	}, nil
}

func (ew *EntryWriter) OverrideAckTimeout(t time.Duration) error {
	ew.mtx.Lock()
	defer ew.mtx.Unlock()
	ew.ackTimeout = t
	if t <= 0 {
		return errors.New("invalid duration")
	}
	return nil
}

func (ew *EntryWriter) Close() (err error) {
	ew.mtx.Lock()
	defer ew.mtx.Unlock()

	if err = ew.forceAckNoLock(); err == nil {
		if err = ew.conn.SetReadDeadline(time.Now().Add(ew.ackTimeout)); err != nil {
			ew.conn.Close()
			ew.hot = false
			return
		}
		//read acks is a liberal implementation which will pull any available
		//acks from the read buffer.  we don't care if we get an error here
		//because this is largely used when trying to refire a connection
		err = ew.readAcks(true)
	}

	ew.conn.Close()
	ew.hot = false
	return
}

func (ew *EntryWriter) ForceAck() error {
	ew.mtx.Lock()
	defer ew.mtx.Unlock()
	return ew.forceAckNoLock()
}

func (ew *EntryWriter) outstandingEntries() []*entry.Entry {
	ew.mtx.Lock()
	defer ew.mtx.Unlock()
	return ew.ecb.outstandingEntries()
}

func (ew *EntryWriter) throwAckSync() error {
	tempBuff := make([]byte, 4) //REMEMBER to adjust this number if the type changes

	//park the magic into our buffer
	binary.LittleEndian.PutUint32(tempBuff, FORCE_ACK_MAGIC)

	//send the buffer and force it out
	if err := ew.writeAll(tempBuff); err != nil {
		return err
	}
	if err := ew.bIO.Flush(); err != nil {
		return err
	}
	return nil
}

// forceAckNoLock sends a signal to the ingester that we want to force out
// and ACK of all outstanding entries.  This is primarily used when
// closing the connection to ensure that all the entries actually
// made it to the ingester. The caller MUST hold the lock
func (ew *EntryWriter) forceAckNoLock() error {
	if err := ew.throwAckSync(); err != nil {
		return err
	}
	//begin servicing acks with blocking and a read deadline
	for ew.ecb.Count() > 0 {
		if err := ew.conn.SetReadDeadline(time.Now().Add(ew.ackTimeout)); err != nil {
			return err
		}
		if err := ew.serviceAcks(true); err != nil {
			ew.conn.SetReadDeadline(time.Time{})
			return err
		}
		ew.conn.SetReadDeadline(time.Time{})
	}
	if ew.ecb.Count() > 0 {
		return fmt.Errorf("Failed to confirm %d entries", ew.ecb.Count())
	}
	return nil
}

// Write expects to have exclusive control over the entry and all
// its buffers from the period of write and forever after.
// This is because it needs to be able to resend the entry if it
// fails to confirm.  If a buffer is re-used and the entry fails
// to confirm we will send the new modified buffer which may not
// have the original data.
func (ew *EntryWriter) Write(ent *entry.Entry) error {
	return ew.writeFlush(ent, false)
}

func (ew *EntryWriter) WriteSync(ent *entry.Entry) error {
	return ew.writeFlush(ent, true)
}

func (ew *EntryWriter) writeFlush(ent *entry.Entry, flush bool) error {
	var err error
	var blocking bool

	ew.mtx.Lock()
	if ew.ecb.Full() {
		blocking = true
	} else {
		blocking = false
	}

	//check if any acks can be serviced
	if err = ew.serviceAcks(blocking); err != nil {
		ew.mtx.Unlock()
		return err
	}

	_, err = ew.writeEntry(ent, flush)
	ew.mtx.Unlock()
	return err
}

// OpenSlots informs the caller how many slots are available before
// we must service acks.  This is used for mostly in a multiplexing
// system where we want to know how much we can write before we need
// to service acks and move on.
func (ew *EntryWriter) OpenSlots(ent *entry.Entry) int {
	ew.mtx.Lock()
	r := ew.ecb.Free()
	ew.mtx.Unlock()
	return r
}

// WriteWithHint behaves exactly like Write but also returns a bool
// which indicates whether or not the a flush was required.  This
// function method is primarily used when muxing across multiple
// indexers, so the muxer knows when to transition to the next indexer
func (ew *EntryWriter) WriteWithHint(ent *entry.Entry) (bool, error) {
	var err error
	var blocking bool

	ew.mtx.Lock()
	defer ew.mtx.Unlock()
	if ew.ecb.Full() {
		blocking = true
	} else {
		blocking = false
	}

	//check if any acks can be serviced
	if err = ew.serviceAcks(blocking); err != nil {
		return false, err
	}
	return ew.writeEntry(ent, true)
}

// WriteBatch takes a slice of entries and writes them,
// this function is useful in multithreaded environments where
// we want to lessen the impact of hits on a channel by threads
func (ew *EntryWriter) WriteBatch(ents [](*entry.Entry)) error {
	var err error

	ew.mtx.Lock()
	defer ew.mtx.Unlock()

	for i := range ents {
		if _, err = ew.writeEntry(ents[i], false); err != nil {
			return err
		}
	}

	return nil
}

func (ew *EntryWriter) writeEntry(ent *entry.Entry, flush bool) (bool, error) {
	var flushed bool
	var err error
	//if our conf buffer is full force an ack service
	if ew.ecb.Full() {
		if err := ew.bIO.Flush(); err != nil {
			return false, err
		}
		if err := ew.serviceAcks(true); err != nil {
			return false, err
		}
	}

	//throw the magic
	binary.LittleEndian.PutUint32(ew.buff, NEW_ENTRY_MAGIC)

	//build out the header with size
	if err = ent.EncodeHeader(ew.buff[4 : entry.ENTRY_HEADER_SIZE+4]); err != nil {
		return false, err
	}
	binary.LittleEndian.PutUint64(ew.buff[entry.ENTRY_HEADER_SIZE+4:], uint64(ew.id))
	//throw it and flush it
	if err = ew.writeAll(ew.buff); err != nil {
		return false, err
	}
	//only flush if we need to
	if len(ent.Data) > ew.bIO.Available() {
		flushed = true
		if err = ew.bIO.Flush(); err != nil {
			return false, err
		}
	}
	//throw the actual data portion and flush it
	if err = ew.writeAll(ent.Data); err != nil {
		return false, err
	}
	if flush {
		flushed = flush
		if err = ew.bIO.Flush(); err != nil {
			return false, err
		}
	}
	if err = ew.ecb.Add(&entryConfirmation{ew.id, ent}); err != nil {
		return false, err
	}
	ew.id++
	return flushed, nil
}

func (ew *EntryWriter) writeAll(b []byte) error {
	var (
		err   error
		n     int
		total int
		tgt   = len(b)
	)
	total = 0
	for total < tgt {
		n, err = ew.bIO.Write(b[total:tgt])
		if err != nil {
			return err
		}
		if n == 0 {
			return errors.New("Failed to write bytes")
		}
		total += n
		if total == tgt {
			break
		}
		//if only a partial write occurred that means we need to flush
		if err = ew.bIO.Flush(); err != nil {
			return err
		}
	}
	return nil
}

// Ack will block waiting for at least one ack to free up a slot for sending
func (ew *EntryWriter) Ack() error {
	ew.mtx.Lock()
	//ensure there are outstanding acks
	if ew.ecb.Count() == 0 {
		ew.mtx.Unlock()
		return nil
	}
	err := ew.serviceAcks(true)
	ew.mtx.Unlock()
	return err
}

// serviceAcks MUST be called with the parent holding the mutex
func (ew *EntryWriter) serviceAcks(blocking bool) error {
	//only flush if we are blocking
	if blocking && ew.bIO.Buffered() > 0 {
		if err := ew.bIO.Flush(); err != nil {
			return err
		}
	}
	//attempt to read acks
	if err := ew.readAcks(blocking); err != nil {
		return err
	}
	if ew.ecb.Full() {
		//if we attempted to read and we are full, force a sync, something is wrong
		if err := ew.throwAckSync(); err != nil {
			return err
		}
		return ew.readAcks(true)
	}
	return nil
}

//readAcks pulls out all of the acks in the ackBuffer and services them
func (ew *EntryWriter) readAcks(blocking bool) error {
	var err error
	var magic uint32
	var id entrySendID
	//loop and service all acks
	//TODO: calculate the full ACK cound and do everything in one read
	//multiple reads are slow
	for ew.ecb.Count() > 0 && (ew.bAckReader.Buffered() >= ACK_SIZE || blocking) {
		/* because we MUST be called with lock already taken we can use
		   the ew buffer for reads and processing */
		if _, err = io.ReadFull(ew.bAckReader, ew.buff[0:ACK_SIZE]); err != nil {
			return err
		}
		//extract the magic and id
		magic = binary.LittleEndian.Uint32(ew.buff[0:])
		id = entrySendID(binary.LittleEndian.Uint64(ew.buff[4:]))
		if magic != CONFIRM_ENTRY_MAGIC {
			//look for it in the other chunks
			if binary.LittleEndian.Uint32(ew.buff[4:]) == CONFIRM_ENTRY_MAGIC {
				//read 4 more bytes and roll with it
				_, err = io.ReadFull(ew.bAckReader, ew.buff[8:16])
				if err != nil {
					return err
				}
				id = entrySendID(binary.LittleEndian.Uint64(ew.buff[8:]))
			} else if binary.LittleEndian.Uint32(ew.buff[8:]) == CONFIRM_ENTRY_MAGIC {
				//read 8 more bytes and roll with it
				_, err = io.ReadFull(ew.bAckReader, ew.buff[12:24])
				if err != nil {
					return err
				}
				id = entrySendID(binary.LittleEndian.Uint64(ew.buff[12:]))
			} else {
				//just continue
				continue
			}
		}
		//check if the ID is the head, if not pop the head and resend
		err = ew.ecb.Confirm(id)
		//TODO: if we get an ID we don't know about we just ignore it
		//      is this the best course of action?
		if err != nil {
			if err != errEntryNotFound {
				return err
			}
		}
		//we set blocking to false because at this point we have serviced an ack
		blocking = false
	}

	return nil
}

func (ew EntryWriter) OptimalBatchWriteSize() int {
	return ew.ecb.Size()
}

func isTimeout(err error) bool {
	nerr, ok := err.(net.Error)
	if !ok {
		return false
	}
	return nerr.Timeout()
}
