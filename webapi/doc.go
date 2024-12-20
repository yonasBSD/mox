// NOTE: DO NOT EDIT, this file is generated by gendoc.sh.

/*
Package webapi implements a simple HTTP/JSON-based API for interacting with
email, and webhooks for notifications about incoming and outgoing deliveries,
including delivery failures.

# Overview

The webapi can be used to compose and send outgoing messages.  The HTTP/JSON
API is often easier to use for developers since it doesn't require separate
libraries and/or having (detailed) knowledge about the format of email messages
("Internet Message Format"), or the SMTP protocol and its extensions.

Webhooks can be configured per account, and help with automated processing of
incoming email, and with handling delivery failures/success.  Webhooks are
often easier to use for developers than monitoring a mailbox with IMAP and
processing new incoming email and delivery status notification (DSN) messages.

# Webapi

The webapi has a base URL at /webapi/v0/ by default, but configurable, which
serves an introduction that points to this documentation and lists the API
methods available.

An HTTP POST to /webapi/v0/<method> calls a method. The form can be either
"application/x-www-form-urlencoded" or "multipart/form-data".  Form field
"request" must contain the request parameters, encoded as JSON.

HTTP basic authentication is required for calling methods, with an email address
as user name. Use a login address configured for "unique SMTP MAIL FROM"
addresses ("FromIDLoginAddresses" in the account configuration), and configure
an interval to "keep retired messages delivered from the queue". This allows
incoming DSNs to be matched to the original outgoing messages, and enables
automatic suppression list management.

HTTP response status 200 OK indicates a successful method call, status 400
indicates an error.  The response body of an error is a JSON object with a
human-readable "Message" field, and a "Code" field for programmatic handling
(common codes: "user" or user-induced errors, "server" for server-caused
errors).  Most successful calls return a JSON object, but some return data
(e.g. a raw message or an attachment of a message). See [Methods] for the
methods and and [Client] for their documentation. The first element of their
return values indicate their JSON object type or io.ReadCloser for non-JSON
data. The request and response types are converted from/to JSON.  Optional and
missing/empty fields/values are converted into Go zero values: zero for
numbers, empty strings, empty lists and empty objects. New fields may be added
in response objects in future versions, parsers should ignore unrecognized
fields.

An HTTP GET to a method URL serves an HTML page showing example
request/response JSON objects in a form and a button to call the method.

# Webhooks

Webhooks for outgoing delivery events and incoming deliveries are configured
per account.

A webhook is delivered by an HTTP POST with headers "X-Mox-Webhook-ID" (unique
ID of webhook) and "X-Mox-Webhook-Attempt" (number of delivery attempts,
starting at 1), and a JSON body with the webhook data.  Failing webhook
deliveries are retried with backoff, each time doubling the interval between
attempts, at 1m, 2m, 4m, 7.5m, 15m and unwards, until the last attempt after a
16h wait period.

See [webhook.Outgoing] for the fields in a webhook for outgoing deliveries, and
in particular [webhook.OutgoingEvent] for the types of events.

Only the latest event for the delivery of a particular outgoing message will be
delivered, any webhooks for that message still in the queue (after failure to
deliver) are retired as superseded when a new event occurs.

Webhooks for incoming deliveries are configured separately from outgoing
deliveries. Incoming DSNs for previously sent messages do not cause a webhook
to the webhook URL for incoming messages, only to the webhook URL for outgoing
delivery events. The incoming webhook JSON payload contains the message
envelope (parsed To, Cc, Bcc, Subject and more headers), the MIME structure,
and the contents of the first text and HTML parts. See [webhook.Incoming] for
the fields in the JSON object. The full message and individual parts, including
attachments, can be retrieved using the webapi.

# Transactional email

When sending transactional emails, potentially to many recipients, it is
important to process delivery failure notifications. If messages are rejected,
or email addresses no longer exist, you should stop sending email to those
addresses. If you try to keep sending, the receiving mail servers may consider
that spammy behaviour and blocklist your mail server.

Automatic suppression list management already prevents most repeated sending
attempts.  The webhooks make it easy to receive failure notifications.

To keep spam complaints about your messages to a minimum, include links to
unsubscribe from future messages without requiring further actions from the
user, such as logins. Include an unsubscribe link in the footer, and include
List-* message headers, such as List-Id, List-Unsubscribe and
List-Unsubscribe-Post.

# Webapi examples

Below are examples for making webapi calls to a locally running "mox
localserve" with its default credentials.

Send a basic message:

	$ curl --user mox@localhost:moxmoxmox \
		--data request='{"To": [{"Address": "mox@localhost"}], "Text": "hi ☺"}' \
		http://localhost:1080/webapi/v0/Send
	{
		"MessageID": "<kVTha0Q-a5Zh1MuTh5rUjg@localhost>",
		"Submissions": [
			{
				"Address": "mox@localhost",
				"QueueMsgID": 10010,
				"FromID": "ZfV16EATHwKEufrSMo055Q"
			}
		]
	}

Send a message with files both from form upload and base64 included in JSON:

	$ curl --user mox@localhost:moxmoxmox \
		--form request='{"To": [{"Address": "mox@localhost"}], "Subject": "hello", "Text": "hi ☺", "HTML": "<img src=\"cid:hi\" />", "AttachedFiles": [{"Name": "img.png", "ContentType": "image/png", "Data": "bWFkZSB5b3UgbG9vayE="}]}' \
		--form 'inlinefile=@hi.png;headers="Content-ID: <hi>"' \
		--form attachedfile=@mox.png \
		http://localhost:1080/webapi/v0/Send
	{
		"MessageID": "<eZ3OEEA2odXovovIxHE49g@localhost>",
		"Submissions": [
			{
				"Address": "mox@localhost",
				"QueueMsgID": 10011,
				"FromID": "yWiUQ6mvJND8FRPSmc9y5A"
			}
		]
	}

Get a message in parsed form:

	$ curl --user mox@localhost:moxmoxmox --data request='{"MsgID": 424}' http://localhost:1080/webapi/v0/MessageGet
	{
		"Message": {
			"From": [
				{
					"Name": "mox",
					"Address": "mox@localhost"
				}
			],
			"To": [
				{
					"Name": "",
					"Address": "mox@localhost"
				}
			],
			"CC": [],
			"BCC": [],
			"ReplyTo": [],
			"MessageID": "<84vCeme_yZXyDzjWDeYBpg@localhost>",
			"References": [],
			"Date": "2024-04-04T14:29:42+02:00",
			"Subject": "hello",
			"Text": "hi \u263a\n",
			"HTML": ""
		},
		"Structure": {
			"ContentType": "multipart/mixed",
			"ContentTypeParams": {
				"boundary": "0ee72dc30dbab2ca6f7a363844a10a9f6111fc6dd31b8ff0b261478c2c48"
			},
			"ContentID": "",
			"DecodedSize": 0,
			"Parts": [
				{
					"ContentType": "multipart/related",
					"ContentTypeParams": {
						"boundary": "b5ed0977ee2b628040f394c3f374012458379a4f3fcda5036371d761c81d"
					},
					"ContentID": "",
					"DecodedSize": 0,
					"Parts": [
						{
							"ContentType": "multipart/alternative",
							"ContentTypeParams": {
								"boundary": "3759771adede7bd191ef37f2aa0e49ff67369f4000c320f198a875e96487"
							},
							"ContentID": "",
							"DecodedSize": 0,
							"Parts": [
								{
									"ContentType": "text/plain",
									"ContentTypeParams": {
										"charset": "utf-8"
									},
									"ContentID": "",
									"DecodedSize": 8,
									"Parts": []
								},
								{
									"ContentType": "text/html",
									"ContentTypeParams": {
										"charset": "us-ascii"
									},
									"ContentID": "",
									"DecodedSize": 22,
									"Parts": []
								}
							]
						},
						{
							"ContentType": "image/png",
							"ContentTypeParams": {},
							"ContentID": "<hi>",
							"DecodedSize": 19375,
							"Parts": []
						}
					]
				},
				{
					"ContentType": "image/png",
					"ContentTypeParams": {},
					"ContentID": "",
					"DecodedSize": 14,
					"Parts": []
				},
				{
					"ContentType": "image/png",
					"ContentTypeParams": {},
					"ContentID": "",
					"DecodedSize": 7766,
					"Parts": []
				}
			]
		},
		"Meta": {
			"Size": 38946,
			"DSN": false,
			"Flags": [
				"$notjunk",
				"\seen"
			],
			"MailFrom": "mox@localhost",
			"RcptTo": "mox@localhost",
			"MailFromValidated": false,
			"MsgFrom": "mox@localhost",
			"MsgFromValidated": false,
			"DKIMVerifiedDomains": [],
			"RemoteIP": "",
			"MailboxName": "Inbox"
		}
	}

Errors (with a 400 bad request HTTP status response) include a human-readable
message and a code for programmatic use:

	$ curl --user mox@localhost:moxmoxmox --data request='{"MsgID": 999}' http://localhost:1080/webapi/v0/MessageGet
	{
		"Code": "notFound",
		"Message": "message not found"
	}

Get a raw, unparsed message, as bytes:

	$ curl --user mox@localhost:moxmoxmox --data request='{"MsgID": 123}' http://localhost:1080/webapi/v0/MessageRawGet
	[message as bytes in raw form]

Mark a message as read and set flag "custom":

	$ curl --user mox@localhost:moxmoxmox --data request='{"MsgID": 424, "Flags": ["\\Seen", "custom"]}' http://localhost:1080/webapi/v0/MessageFlagsAdd
	{}

# Webhook examples

A webhook is delivered by an HTTP POST, wich headers X-Mox-Webhook-ID and
X-Mox-Webhook-Attempt and a JSON body with the data. To simulate a webhook call
for incoming messages, use:

	curl -H 'X-Mox-Webhook-ID: 123' -H 'X-Mox-Webhook-Attempt: 1' --json '{...}' http://localhost/yourapp

Example webhook HTTP POST JSON body for successful outgoing delivery:

	{
		"Version": 0,
		"Event": "delivered",
		"DSN": false,
		"Suppressing": false,
		"QueueMsgID": 101,
		"FromID": "MDEyMzQ1Njc4OWFiY2RlZg",
		"MessageID": "<QnxzgulZK51utga6agH_rg@mox.example>",
		"Subject": "subject of original message",
		"WebhookQueued": "2024-03-27T00:00:00Z",
		"SMTPCode": 250,
		"SMTPEnhancedCode": "",
		"Error": "",
		"Extra": {}
	}

Example webhook HTTP POST JSON body for failed delivery based on incoming DSN
message, with custom extra data fields (from original submission), and adding address to the suppression list:

	{
		"Version": 0,
		"Event": "failed",
		"DSN": true,
		"Suppressing": true,
		"QueueMsgID": 102,
		"FromID": "MDEyMzQ1Njc4OWFiY2RlZg",
		"MessageID": "<QnxzgulZK51utga6agH_rg@mox.example>",
		"Subject": "subject of original message",
		"WebhookQueued": "2024-03-27T00:00:00Z",
		"SMTPCode": 554,
		"SMTPEnhancedCode": "5.4.0",
		"Error": "timeout connecting to host",
		"Extra": {
			"userid": "456"
		}
	}

Example JSON body for webhooks for incoming delivery of basic message:

	{
		"Version": 0,
		"From": [
			{
				"Name": "",
				"Address": "mox@localhost"
			}
		],
		"To": [
			{
				"Name": "",
				"Address": "mjl@localhost"
			}
		],
		"CC": [],
		"BCC": [],
		"ReplyTo": [],
		"Subject": "hi",
		"MessageID": "<QnxzgulZK51utga6agH_rg@mox.example>",
		"InReplyTo": "",
		"References": [],
		"Date": "2024-03-27T00:00:00Z",
		"Text": "hello world ☺\n",
		"HTML": "",
		"Structure": {
			"ContentType": "text/plain",
			"ContentTypeParams": {
				"charset": "utf-8"
			},
			"ContentID": "",
			"ContentDisposition": "",
			"Filename": "",
			"DecodedSize": 17,
			"Parts": []
		},
		"Meta": {
			"MsgID": 201,
			"MailFrom": "mox@localhost",
			"MailFromValidated": false,
			"MsgFromValidated": true,
			"RcptTo": "mjl@localhost",
			"DKIMVerifiedDomains": [
				"localhost"
			],
			"RemoteIP": "127.0.0.1",
			"Received": "2024-03-27T00:00:03Z",
			"MailboxName": "Inbox",
			"Automated": false
		}
	}
*/
package webapi

// NOTE: DO NOT EDIT, this file is generated by gendoc.sh.
