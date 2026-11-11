import datetime

from google.protobuf import struct_pb2 as _struct_pb2
from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class State(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    STATE_UNSPECIFIED: _ClassVar[State]
    STATE_IDLE: _ClassVar[State]
    STATE_CHECKING: _ClassVar[State]
    STATE_PAUSED: _ClassVar[State]
    STATE_FAILING: _ClassVar[State]

class Severity(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    SEVERITY_UNSPECIFIED: _ClassVar[Severity]
    SEVERITY_INFO: _ClassVar[Severity]
    SEVERITY_URGENT: _ClassVar[Severity]
STATE_UNSPECIFIED: State
STATE_IDLE: State
STATE_CHECKING: State
STATE_PAUSED: State
STATE_FAILING: State
SEVERITY_UNSPECIFIED: Severity
SEVERITY_INFO: Severity
SEVERITY_URGENT: Severity

class DescribeRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class DescribeResponse(_message.Message):
    __slots__ = ("name", "display_name", "version", "kinds")
    NAME_FIELD_NUMBER: _ClassVar[int]
    DISPLAY_NAME_FIELD_NUMBER: _ClassVar[int]
    VERSION_FIELD_NUMBER: _ClassVar[int]
    KINDS_FIELD_NUMBER: _ClassVar[int]
    name: str
    display_name: str
    version: str
    kinds: _containers.RepeatedCompositeFieldContainer[EventKind]
    def __init__(self, name: _Optional[str] = ..., display_name: _Optional[str] = ..., version: _Optional[str] = ..., kinds: _Optional[_Iterable[_Union[EventKind, _Mapping]]] = ...) -> None: ...

class EventKind(_message.Message):
    __slots__ = ("id", "description")
    ID_FIELD_NUMBER: _ClassVar[int]
    DESCRIPTION_FIELD_NUMBER: _ClassVar[int]
    id: str
    description: str
    def __init__(self, id: _Optional[str] = ..., description: _Optional[str] = ...) -> None: ...

class StatusRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class StatusResponse(_message.Message):
    __slots__ = ("state", "message", "last_check_started_at", "last_success_at", "next_check_at")
    STATE_FIELD_NUMBER: _ClassVar[int]
    MESSAGE_FIELD_NUMBER: _ClassVar[int]
    LAST_CHECK_STARTED_AT_FIELD_NUMBER: _ClassVar[int]
    LAST_SUCCESS_AT_FIELD_NUMBER: _ClassVar[int]
    NEXT_CHECK_AT_FIELD_NUMBER: _ClassVar[int]
    state: State
    message: str
    last_check_started_at: _timestamp_pb2.Timestamp
    last_success_at: _timestamp_pb2.Timestamp
    next_check_at: _timestamp_pb2.Timestamp
    def __init__(self, state: _Optional[_Union[State, str]] = ..., message: _Optional[str] = ..., last_check_started_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., last_success_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., next_check_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class CheckRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class CheckResponse(_message.Message):
    __slots__ = ("accepted", "message")
    ACCEPTED_FIELD_NUMBER: _ClassVar[int]
    MESSAGE_FIELD_NUMBER: _ClassVar[int]
    accepted: bool
    message: str
    def __init__(self, accepted: bool = ..., message: _Optional[str] = ...) -> None: ...

class EventsRequest(_message.Message):
    __slots__ = ("after", "limit")
    AFTER_FIELD_NUMBER: _ClassVar[int]
    LIMIT_FIELD_NUMBER: _ClassVar[int]
    after: int
    limit: int
    def __init__(self, after: _Optional[int] = ..., limit: _Optional[int] = ...) -> None: ...

class EventsResponse(_message.Message):
    __slots__ = ("events",)
    EVENTS_FIELD_NUMBER: _ClassVar[int]
    events: _containers.RepeatedCompositeFieldContainer[Event]
    def __init__(self, events: _Optional[_Iterable[_Union[Event, _Mapping]]] = ...) -> None: ...

class Event(_message.Message):
    __slots__ = ("seq", "id", "kind", "severity", "occurred_at", "title", "body", "url", "payload")
    SEQ_FIELD_NUMBER: _ClassVar[int]
    ID_FIELD_NUMBER: _ClassVar[int]
    KIND_FIELD_NUMBER: _ClassVar[int]
    SEVERITY_FIELD_NUMBER: _ClassVar[int]
    OCCURRED_AT_FIELD_NUMBER: _ClassVar[int]
    TITLE_FIELD_NUMBER: _ClassVar[int]
    BODY_FIELD_NUMBER: _ClassVar[int]
    URL_FIELD_NUMBER: _ClassVar[int]
    PAYLOAD_FIELD_NUMBER: _ClassVar[int]
    seq: int
    id: str
    kind: str
    severity: Severity
    occurred_at: _timestamp_pb2.Timestamp
    title: str
    body: str
    url: str
    payload: _struct_pb2.Struct
    def __init__(self, seq: _Optional[int] = ..., id: _Optional[str] = ..., kind: _Optional[str] = ..., severity: _Optional[_Union[Severity, str]] = ..., occurred_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., title: _Optional[str] = ..., body: _Optional[str] = ..., url: _Optional[str] = ..., payload: _Optional[_Union[_struct_pb2.Struct, _Mapping]] = ...) -> None: ...
