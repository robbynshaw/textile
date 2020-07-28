package client

import (
	"context"
	"fmt"
	"time"

	"github.com/textileio/go-threads/core/thread"
	pb "github.com/textileio/textile/api/users/pb"
	"google.golang.org/grpc"
)

// Client provides the client api.
type Client struct {
	c    pb.APIClient
	conn *grpc.ClientConn
}

// NewClient starts the client.
func NewClient(target string, opts ...grpc.DialOption) (*Client, error) {
	conn, err := grpc.Dial(target, opts...)
	if err != nil {
		return nil, err
	}
	return &Client{
		c:    pb.NewAPIClient(conn),
		conn: conn,
	}, nil
}

// Close closes the client's grpc connection and cancels any active requests.
func (c *Client) Close() error {
	return c.conn.Close()
}

// GetThread returns a thread by name.
func (c *Client) GetThread(ctx context.Context, name string) (*pb.GetThreadReply, error) {
	return c.c.GetThread(ctx, &pb.GetThreadRequest{
		Name: name,
	})
}

// ListThreads returns a list of threads.
// Threads can be created using the threads or threads network client.
func (c *Client) ListThreads(ctx context.Context) (*pb.ListThreadsReply, error) {
	return c.c.ListThreads(ctx, &pb.ListThreadsRequest{})
}

// SetupMailbox creates inbox and sentbox threads needed user mail.
func (c *Client) SetupMail(ctx context.Context) (inbox, sentbox thread.ID, err error) {
	res, err := c.c.SetupMail(ctx, &pb.SetupMailRequest{})
	if err != nil {
		return
	}
	inbox, err = thread.Cast(res.InboxID)
	if err != nil {
		return
	}
	sentbox, err = thread.Cast(res.SentboxID)
	if err != nil {
		return
	}
	return
}

// Message is the client side representation of a mailbox message.
type Message struct {
	ID        string        `json:"_id"`
	From      thread.PubKey `json:"from"`
	To        thread.PubKey `json:"to"`
	Body      []byte        `json:"body"`
	Signature []byte        `json:"body"`
	CreatedAt time.Time     `json:"created_at"`
	ReadAt    time.Time     `json:"read_at,omitempty"`
}

// SendMessage sends the message body to a recipient.
func (c *Client) SendMessage(ctx context.Context, from thread.Identity, to thread.PubKey, body []byte) (msg Message, err error) {
	cbody, err := to.Encrypt(body)
	if err != nil {
		return msg, err
	}
	sig, err := from.Sign(ctx, cbody)
	if err != nil {
		return msg, err
	}
	res, err := c.c.SendMessage(ctx, &pb.SendMessageRequest{
		To:        to.String(),
		Body:      cbody,
		Signature: sig,
	})
	if err != nil {
		return msg, err
	}
	return Message{
		ID:        res.ID,
		From:      from.GetPublic(),
		To:        to,
		Body:      cbody,
		Signature: sig,
		CreatedAt: time.Unix(0, res.CreatedAt),
	}, nil
}

// ListInboxMessages lists messages from the inbox.
// Use options to paginate with seek and limit,
// and filter by read status.
func (c *Client) ListInboxMessages(ctx context.Context, to thread.Identity, opts ...ListOption) ([]Message, error) {
	args := &listOptions{
		status: All,
	}
	for _, opt := range opts {
		opt(args)
	}
	res, err := c.c.ListInboxMessages(ctx, &pb.ListInboxMessagesRequest{
		Seek:   args.seek,
		Limit:  int64(args.limit),
		Status: pb.ListInboxMessagesRequest_Status(args.status),
	})
	if err != nil {
		return nil, err
	}
	return handleMessageList(ctx, res, to)
}

// ListSentMessages lists messages from the sentbox.
// Use options to paginate with seek and limit.
func (c *Client) ListSentMessages(ctx context.Context, from thread.Identity, opts ...ListOption) ([]Message, error) {
	args := &listOptions{
		status: All,
	}
	for _, opt := range opts {
		opt(args)
	}
	res, err := c.c.ListSentMessages(ctx, &pb.ListSentMessagesRequest{
		Seek:  args.seek,
		Limit: int64(args.limit),
	})
	if err != nil {
		return nil, err
	}
	return handleMessageList(ctx, res, from, nil)
}

func handleMessageList(ctx context.Context, res *pb.ListMessagesReply, to thread.Identity) ([]Message, error) {
	msgs := make([]Message, len(res.Messages))
	var err error
	for i, m := range res.Messages {
		msgs[i], err = messageFromPb(ctx, m, to)
		if err != nil {
			return nil, err
		}
	}
	return msgs, nil
}

func inboxMsgFromPb(ctx context.Context, m *pb.Message, to thread.Identity) (msg Message, err error) {
	from := &thread.Libp2pPubKey{}
	if err := from.UnmarshalString(m.From); err != nil {
		return msg, fmt.Errorf("invalid public key")
	}
	ok, err := from.Verify(m.Body, m.Signature)
	if !ok || err != nil {
		return msg, fmt.Errorf("bad message signature")
	}
	body, err := to.Decrypt(ctx, m.Body)
	if err != nil {
		return msg, err
	}
	readAt := time.Time{}
	if m.ReadAt > 0 {
		readAt = time.Unix(0, m.ReadAt)
	}
	return Message{
		ID:        m.ID,
		From:      from,
		To:        to.GetPublic(),
		Body:      body,
		Signature: m.Signature,
		CreatedAt: time.Unix(0, m.CreatedAt),
		ReadAt:    readAt,
	}, nil
}

func sentMsgFromPb(ctx context.Context, m *pb.Message, from thread.Identity) (msg Message, err error) {
	to := &thread.Libp2pPubKey{}
	if err := to.UnmarshalString(m.From); err != nil {
		return msg, fmt.Errorf("invalid public key")
	}
	ok, err := to.Verify(m.Body, m.Signature)
	if !ok || err != nil {
		return msg, fmt.Errorf("bad message signature")
	}
	readAt := time.Time{}
	if m.ReadAt > 0 {
		readAt = time.Unix(0, m.ReadAt)
	}
	return Message{
		ID:        m.ID,
		From:      from,
		To:        to.GetPublic(),
		Body:      body,
		Signature: m.Signature,
		CreatedAt: time.Unix(0, m.CreatedAt),
		ReadAt:    readAt,
	}, nil
}

// ReadInboxMessage marks a message as read by ID.
func (c *Client) ReadInboxMessage(ctx context.Context, id string) error {
	_, err := c.c.ReadInboxMessage(ctx, &pb.ReadMessageRequest{
		ID: id,
	})
	return err
}

// DeleteInboxMessage deletes an inbox message by ID.
func (c *Client) DeleteInboxMessage(ctx context.Context, id string) error {
	_, err := c.c.DeleteInboxMessage(ctx, &pb.DeleteMessageRequest{
		ID: id,
	})
	return err
}

// DeleteSentMessage deletes a sent message by ID.
func (c *Client) DeleteSentMessage(ctx context.Context, id string) error {
	_, err := c.c.DeleteSentMessage(ctx, &pb.DeleteMessageRequest{
		ID: id,
	})
	return err
}
