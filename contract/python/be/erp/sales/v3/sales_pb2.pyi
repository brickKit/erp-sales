from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class ClaimState(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    CLAIM_STATE_UNSPECIFIED: _ClassVar[ClaimState]
    CLAIM_STATE_PENDING: _ClassVar[ClaimState]
    CLAIM_STATE_CONFIRMED: _ClassVar[ClaimState]
    CLAIM_STATE_FORMALIZED: _ClassVar[ClaimState]
    CLAIM_STATE_EXPIRED: _ClassVar[ClaimState]
    CLAIM_STATE_RELEASED: _ClassVar[ClaimState]

class ReadState(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    READ_STATE_UNSPECIFIED: _ClassVar[ReadState]
    READ_STATE_EXISTS: _ClassVar[ReadState]
    READ_STATE_NOT_FOUND: _ClassVar[ReadState]
CLAIM_STATE_UNSPECIFIED: ClaimState
CLAIM_STATE_PENDING: ClaimState
CLAIM_STATE_CONFIRMED: ClaimState
CLAIM_STATE_FORMALIZED: ClaimState
CLAIM_STATE_EXPIRED: ClaimState
CLAIM_STATE_RELEASED: ClaimState
READ_STATE_UNSPECIFIED: ReadState
READ_STATE_EXISTS: ReadState
READ_STATE_NOT_FOUND: ReadState

class Measure(_message.Message):
    __slots__ = ("quantity", "amount")
    QUANTITY_FIELD_NUMBER: _ClassVar[int]
    AMOUNT_FIELD_NUMBER: _ClassVar[int]
    quantity: str
    amount: str
    def __init__(self, quantity: _Optional[str] = ..., amount: _Optional[str] = ...) -> None: ...

class LedgerLine(_message.Message):
    __slots__ = ("order_line_id", "billable", "claimed", "billed")
    ORDER_LINE_ID_FIELD_NUMBER: _ClassVar[int]
    BILLABLE_FIELD_NUMBER: _ClassVar[int]
    CLAIMED_FIELD_NUMBER: _ClassVar[int]
    BILLED_FIELD_NUMBER: _ClassVar[int]
    order_line_id: str
    billable: Measure
    claimed: Measure
    billed: Measure
    def __init__(self, order_line_id: _Optional[str] = ..., billable: _Optional[_Union[Measure, _Mapping]] = ..., claimed: _Optional[_Union[Measure, _Mapping]] = ..., billed: _Optional[_Union[Measure, _Mapping]] = ...) -> None: ...

class LedgerRequest(_message.Message):
    __slots__ = ("order_id",)
    ORDER_ID_FIELD_NUMBER: _ClassVar[int]
    order_id: str
    def __init__(self, order_id: _Optional[str] = ...) -> None: ...

class LedgerResponse(_message.Message):
    __slots__ = ("order_id", "order_state", "lines", "order_version")
    ORDER_ID_FIELD_NUMBER: _ClassVar[int]
    ORDER_STATE_FIELD_NUMBER: _ClassVar[int]
    LINES_FIELD_NUMBER: _ClassVar[int]
    ORDER_VERSION_FIELD_NUMBER: _ClassVar[int]
    order_id: str
    order_state: str
    lines: _containers.RepeatedCompositeFieldContainer[LedgerLine]
    order_version: int
    def __init__(self, order_id: _Optional[str] = ..., order_state: _Optional[str] = ..., lines: _Optional[_Iterable[_Union[LedgerLine, _Mapping]]] = ..., order_version: _Optional[int] = ...) -> None: ...

class ClaimLine(_message.Message):
    __slots__ = ("order_line_id", "measure")
    ORDER_LINE_ID_FIELD_NUMBER: _ClassVar[int]
    MEASURE_FIELD_NUMBER: _ClassVar[int]
    order_line_id: str
    measure: Measure
    def __init__(self, order_line_id: _Optional[str] = ..., measure: _Optional[_Union[Measure, _Mapping]] = ...) -> None: ...

class Claim(_message.Message):
    __slots__ = ("claim_id", "order_id", "state", "lines", "billed", "expires_at")
    CLAIM_ID_FIELD_NUMBER: _ClassVar[int]
    ORDER_ID_FIELD_NUMBER: _ClassVar[int]
    STATE_FIELD_NUMBER: _ClassVar[int]
    LINES_FIELD_NUMBER: _ClassVar[int]
    BILLED_FIELD_NUMBER: _ClassVar[int]
    EXPIRES_AT_FIELD_NUMBER: _ClassVar[int]
    claim_id: str
    order_id: str
    state: ClaimState
    lines: _containers.RepeatedCompositeFieldContainer[ClaimLine]
    billed: _containers.RepeatedCompositeFieldContainer[ClaimLine]
    expires_at: str
    def __init__(self, claim_id: _Optional[str] = ..., order_id: _Optional[str] = ..., state: _Optional[_Union[ClaimState, str]] = ..., lines: _Optional[_Iterable[_Union[ClaimLine, _Mapping]]] = ..., billed: _Optional[_Iterable[_Union[ClaimLine, _Mapping]]] = ..., expires_at: _Optional[str] = ...) -> None: ...

class ClaimRequest(_message.Message):
    __slots__ = ("claim_id", "order_id", "lines")
    CLAIM_ID_FIELD_NUMBER: _ClassVar[int]
    ORDER_ID_FIELD_NUMBER: _ClassVar[int]
    LINES_FIELD_NUMBER: _ClassVar[int]
    claim_id: str
    order_id: str
    lines: _containers.RepeatedCompositeFieldContainer[ClaimLine]
    def __init__(self, claim_id: _Optional[str] = ..., order_id: _Optional[str] = ..., lines: _Optional[_Iterable[_Union[ClaimLine, _Mapping]]] = ...) -> None: ...

class ClaimProblem(_message.Message):
    __slots__ = ("order_line_id", "reason", "remaining")
    ORDER_LINE_ID_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    REMAINING_FIELD_NUMBER: _ClassVar[int]
    order_line_id: str
    reason: str
    remaining: Measure
    def __init__(self, order_line_id: _Optional[str] = ..., reason: _Optional[str] = ..., remaining: _Optional[_Union[Measure, _Mapping]] = ...) -> None: ...

class ClaimResponse(_message.Message):
    __slots__ = ("claimed", "claim", "problems")
    CLAIMED_FIELD_NUMBER: _ClassVar[int]
    CLAIM_FIELD_NUMBER: _ClassVar[int]
    PROBLEMS_FIELD_NUMBER: _ClassVar[int]
    claimed: bool
    claim: Claim
    problems: _containers.RepeatedCompositeFieldContainer[ClaimProblem]
    def __init__(self, claimed: _Optional[bool] = ..., claim: _Optional[_Union[Claim, _Mapping]] = ..., problems: _Optional[_Iterable[_Union[ClaimProblem, _Mapping]]] = ...) -> None: ...

class RenewRequest(_message.Message):
    __slots__ = ("claim_id",)
    CLAIM_ID_FIELD_NUMBER: _ClassVar[int]
    claim_id: str
    def __init__(self, claim_id: _Optional[str] = ...) -> None: ...

class RenewResponse(_message.Message):
    __slots__ = ("claim",)
    CLAIM_FIELD_NUMBER: _ClassVar[int]
    claim: Claim
    def __init__(self, claim: _Optional[_Union[Claim, _Mapping]] = ...) -> None: ...

class ConfirmRequest(_message.Message):
    __slots__ = ("claim_id",)
    CLAIM_ID_FIELD_NUMBER: _ClassVar[int]
    claim_id: str
    def __init__(self, claim_id: _Optional[str] = ...) -> None: ...

class ConfirmResponse(_message.Message):
    __slots__ = ("confirmed", "claim")
    CONFIRMED_FIELD_NUMBER: _ClassVar[int]
    CLAIM_FIELD_NUMBER: _ClassVar[int]
    confirmed: bool
    claim: Claim
    def __init__(self, confirmed: _Optional[bool] = ..., claim: _Optional[_Union[Claim, _Mapping]] = ...) -> None: ...

class FormalizeRequest(_message.Message):
    __slots__ = ("claim_id", "billed")
    CLAIM_ID_FIELD_NUMBER: _ClassVar[int]
    BILLED_FIELD_NUMBER: _ClassVar[int]
    claim_id: str
    billed: _containers.RepeatedCompositeFieldContainer[ClaimLine]
    def __init__(self, claim_id: _Optional[str] = ..., billed: _Optional[_Iterable[_Union[ClaimLine, _Mapping]]] = ...) -> None: ...

class FormalizeResponse(_message.Message):
    __slots__ = ("claim",)
    CLAIM_FIELD_NUMBER: _ClassVar[int]
    claim: Claim
    def __init__(self, claim: _Optional[_Union[Claim, _Mapping]] = ...) -> None: ...

class ReleaseRequest(_message.Message):
    __slots__ = ("claim_id",)
    CLAIM_ID_FIELD_NUMBER: _ClassVar[int]
    claim_id: str
    def __init__(self, claim_id: _Optional[str] = ...) -> None: ...

class ReleaseResponse(_message.Message):
    __slots__ = ("claim",)
    CLAIM_FIELD_NUMBER: _ClassVar[int]
    claim: Claim
    def __init__(self, claim: _Optional[_Union[Claim, _Mapping]] = ...) -> None: ...

class ReverseRequest(_message.Message):
    __slots__ = ("claim_id", "correction_id", "lines")
    CLAIM_ID_FIELD_NUMBER: _ClassVar[int]
    CORRECTION_ID_FIELD_NUMBER: _ClassVar[int]
    LINES_FIELD_NUMBER: _ClassVar[int]
    claim_id: str
    correction_id: str
    lines: _containers.RepeatedCompositeFieldContainer[ClaimLine]
    def __init__(self, claim_id: _Optional[str] = ..., correction_id: _Optional[str] = ..., lines: _Optional[_Iterable[_Union[ClaimLine, _Mapping]]] = ...) -> None: ...

class ReverseResponse(_message.Message):
    __slots__ = ("claim",)
    CLAIM_FIELD_NUMBER: _ClassVar[int]
    claim: Claim
    def __init__(self, claim: _Optional[_Union[Claim, _Mapping]] = ...) -> None: ...

class ReadRequest(_message.Message):
    __slots__ = ("ids",)
    IDS_FIELD_NUMBER: _ClassVar[int]
    ids: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, ids: _Optional[_Iterable[str]] = ...) -> None: ...

class ReadResult(_message.Message):
    __slots__ = ("id", "state", "version", "data")
    ID_FIELD_NUMBER: _ClassVar[int]
    STATE_FIELD_NUMBER: _ClassVar[int]
    VERSION_FIELD_NUMBER: _ClassVar[int]
    DATA_FIELD_NUMBER: _ClassVar[int]
    id: str
    state: ReadState
    version: int
    data: bytes
    def __init__(self, id: _Optional[str] = ..., state: _Optional[_Union[ReadState, str]] = ..., version: _Optional[int] = ..., data: _Optional[bytes] = ...) -> None: ...

class ReadResponse(_message.Message):
    __slots__ = ("results",)
    RESULTS_FIELD_NUMBER: _ClassVar[int]
    results: _containers.RepeatedCompositeFieldContainer[ReadResult]
    def __init__(self, results: _Optional[_Iterable[_Union[ReadResult, _Mapping]]] = ...) -> None: ...
