# Remote protocol transcripts

Each `v<N>/` directory records protocol version N's calls: for every call, the
request a client of that version wrote and the reply frames a server of that
version answered, one JSON file per call.

`TestRemoteProtocolTranscripts` replays every recorded version in both roles,
on every build:

- **server role**: the current server answers each recorded request, and every
  field the recorded reply carries must be present in the current reply with the
  same value, so a client of that version still reads it;
- **client role**: the current client accepts each recorded reply and decodes
  every recorded field, and its own request carries every field the recorded
  request did, so a server of that version still reads it.

**Recorded transcripts are immutable.** Never edit or delete a file here, not
even to fix a typo: an old peer in the field speaks exactly what is recorded. A
protocol change adds a new `v<N+1>/` directory, recorded from the build that
introduces it, and leaves every older one in place.

## File format

```json
{
  "version": 1,
  "op": "hello",
  "request": { "...": "the request frame" },
  "replies": [ { "...": "each reply frame, in order" } ]
}
```

The last reply is the call's result or error; any before it are progress
frames, whose values depend on timing, so only their shape is compared.
