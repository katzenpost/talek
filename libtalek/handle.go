package libtalek

import (
	"encoding/binary"
	"errors"
	"io"

	"github.com/agl/ed25519"
	"github.com/dchest/siphash"
	"github.com/privacylab/talek/common"
	"github.com/privacylab/talek/drbg"
	"golang.org/x/crypto/nacl/box"
)

// Handle is the readable component of a Talek Log.
// Handles are created by making a NewTopic, but can be independently
// shared, and restored from a serialized state. A Handle is read
// by calling Client.Poll(handle) to receive a channel with new messages
// read from the Handle.
type Handle struct {
	// for random looking pir requests
	drbg *drbg.HashDrbg

	// For learning log positions
	Seed1 *drbg.Seed
	Seed2 *drbg.Seed

	// For decrypting messages
	SharedSecret     *[32]byte
	SigningPublicKey *[32]byte

	// Current log position
	Seqno uint64

	// Notifications of new messages
	updates chan []byte

	// log for messages
	log *common.Logger
}

//NewHandle creates a new topic handle, without attachment to a specific topic.
func NewHandle() (h *Handle, err error) {
	h = &Handle{}
	err = initHandle(h)
	return
}

func initHandle(h *Handle) (err error) {
	h.updates = make(chan []byte)

	h.drbg, err = drbg.NewHashDrbg(nil)
	return
}

// nextBuckets returns the pair of buckets that will be used in the next poll or publish of this
// topic given the current sequence number of the handle.
// The buckets returned by this method must still be wrapped by the NumBuckets config
// parameter of talek instance it is requested against.
func (h *Handle) nextBuckets(conf *common.Config) (uint64, uint64) {
	seqNoBytes := make([]byte, 24)
	_ = binary.PutUvarint(seqNoBytes, h.Seqno)

	k0, k1 := h.Seed1.KeyUint128()
	b1 := siphash.Hash(k0, k1, seqNoBytes)
	k0, k1 = h.Seed2.KeyUint128()
	b2 := siphash.Hash(k0, k1, seqNoBytes)

	return b1 % conf.NumBuckets, b2 % conf.NumBuckets
}

func makeReadArg(config *ClientConfig, bucket uint64, rand io.Reader) *common.ReadArgs {
	arg := &common.ReadArgs{}
	num := len(config.TrustDomains)
	arg.TD = make([]common.PirArgs, num)
	arg.TD[0].RequestVector = make([]byte, (config.Config.NumBuckets+7)/8)
	arg.TD[0].RequestVector[bucket/8] |= 1 << (bucket % 8)
	arg.TD[0].PadSeed = make([]byte, drbg.SeedLength)
	if _, err := rand.Read(arg.TD[0].PadSeed); err != nil {
		return nil
	}

	for j := 1; j < num; j++ {
		arg.TD[j].RequestVector = make([]byte, (config.Config.NumBuckets+7)/8)
		if _, err := rand.Read(arg.TD[j].RequestVector); err != nil {
			return nil
		}
		arg.TD[j].PadSeed = make([]byte, drbg.SeedLength)
		if _, err := rand.Read(arg.TD[j].PadSeed); err != nil {
			return nil
		}
		for k := 0; k < len(arg.TD[j].RequestVector); k++ {
			arg.TD[0].RequestVector[k] ^= arg.TD[j].RequestVector[k]
		}
	}
	return arg
}

func (h *Handle) generatePoll(config *ClientConfig, rand io.Reader) (*common.ReadArgs, *common.ReadArgs, error) {
	if h.SharedSecret == nil || h.SigningPublicKey == nil {
		return nil, nil, errors.New("Subscription not fully initialized")
	}

	args := make([]*common.ReadArgs, 2)
	bucket1, bucket2 := h.nextBuckets(config.Config)

	args[0] = makeReadArg(config, bucket1, rand)
	args[1] = makeReadArg(config, bucket2, rand)

	return args[0], args[1], nil
}

// Decrypt attempts decryption of a message for a topic using a specific nonce.
func (h *Handle) Decrypt(cyphertext []byte, nonce *[24]byte) ([]byte, error) {
	if h.SharedSecret == nil || h.SigningPublicKey == nil {
		return nil, errors.New("Handle improperly initialized")
	}
	cypherlen := len(cyphertext)
	if cypherlen < ed25519.SignatureSize {
		return nil, errors.New("Invalid cyphertext")
	}

	//verify signature
	message := cyphertext[0 : cypherlen-ed25519.SignatureSize]
	var sig [ed25519.SignatureSize]byte
	copy(sig[:], cyphertext[cypherlen-ed25519.SignatureSize:])
	if !ed25519.Verify(h.SigningPublicKey, message, &sig) {
		return nil, errors.New("Invalid Signature")
	}

	//decrypt
	plaintext := make([]byte, 0, cypherlen-box.Overhead-ed25519.SignatureSize)
	_, ok := box.OpenAfterPrecomputation(plaintext, message, nonce, h.SharedSecret)
	if !ok {
		return nil, errors.New("Failed to decrypt")
	}
	return plaintext[0:cap(plaintext)], nil
}

// OnResponse processes a response for a request generated by generatePoll,
// sending it to the handle's updates channel if valid.
func (h *Handle) OnResponse(args *common.ReadArgs, reply *common.ReadReply, dataSize uint) {
	msg := h.retrieveResponse(args, reply, dataSize)
	if msg != nil && h.updates != nil {
		h.Seqno++
		h.updates <- msg
	}
}

func (h *Handle) retrieveResponse(args *common.ReadArgs, reply *common.ReadReply, dataSize uint) []byte {
	data := reply.Data

	// strip out the padding injected by trust domains.
	for i := 0; i < len(args.TD); i++ {
		if err := drbg.Overlay(args.TD[i].PadSeed, data); err != nil {
			if h.log != nil {
				h.log.Info.Printf("Failed to remove pad on returned read: %v\n", err)
			}
			return nil
		}
	}

	var seqNoBytes [24]byte
	_ = binary.PutUvarint(seqNoBytes[:], h.Seqno)

	// A 'bucket' likely has multiple messages in it. See if any of them are ours.
	for i := uint(0); i < uint(len(data)); i += dataSize {
		plaintext, err := h.Decrypt(data[i:i+dataSize], &seqNoBytes)
		if err == nil {
			if h.log != nil {
				h.log.Trace.Printf("Successful Decryption.\n")
			}
			return plaintext
		}

		if h.log != nil {
			h.log.Trace.Printf("decryption failed for read %d of bucket %d [%v](%d): %v\n",
				i/dataSize,
				args.Bucket(),
				data[i:i+4],
				len(data[i:i+dataSize]),
				err)
		}
	}
	return nil
}
