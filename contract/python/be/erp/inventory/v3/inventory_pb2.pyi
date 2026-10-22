from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class BranchState(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    BRANCH_STATE_UNSPECIFIED: _ClassVar[BranchState]
    BRANCH_STATE_HELD: _ClassVar[BranchState]
    BRANCH_STATE_COMMITTED: _ClassVar[BranchState]
    BRANCH_STATE_CONSUMED: _ClassVar[BranchState]
    BRANCH_STATE_RELEASED: _ClassVar[BranchState]
    BRANCH_STATE_EXPIRED: _ClassVar[BranchState]
    BRANCH_STATE_REJECTED: _ClassVar[BranchState]
    BRANCH_STATE_CANCELLED: _ClassVar[BranchState]
    BRANCH_STATE_ABSENT: _ClassVar[BranchState]
BRANCH_STATE_UNSPECIFIED: BranchState
BRANCH_STATE_HELD: BranchState
BRANCH_STATE_COMMITTED: BranchState
BRANCH_STATE_CONSUMED: BranchState
BRANCH_STATE_RELEASED: BranchState
BRANCH_STATE_EXPIRED: BranchState
BRANCH_STATE_REJECTED: BranchState
BRANCH_STATE_CANCELLED: BranchState
BRANCH_STATE_ABSENT: BranchState

class Reference(_message.Message):
    __slots__ = ("kind", "id", "revision")
    KIND_FIELD_NUMBER: _ClassVar[int]
    ID_FIELD_NUMBER: _ClassVar[int]
    REVISION_FIELD_NUMBER: _ClassVar[int]
    kind: str
    id: str
    revision: int
    def __init__(self, kind: _Optional[str] = ..., id: _Optional[str] = ..., revision: _Optional[int] = ...) -> None: ...

class HoldLine(_message.Message):
    __slots__ = ("product_id", "warehouse_id", "quantity")
    PRODUCT_ID_FIELD_NUMBER: _ClassVar[int]
    WAREHOUSE_ID_FIELD_NUMBER: _ClassVar[int]
    QUANTITY_FIELD_NUMBER: _ClassVar[int]
    product_id: str
    warehouse_id: str
    quantity: str
    def __init__(self, product_id: _Optional[str] = ..., warehouse_id: _Optional[str] = ..., quantity: _Optional[str] = ...) -> None: ...

class HoldRequest(_message.Message):
    __slots__ = ("branch_id", "legal_entity_id", "reference", "lines")
    BRANCH_ID_FIELD_NUMBER: _ClassVar[int]
    LEGAL_ENTITY_ID_FIELD_NUMBER: _ClassVar[int]
    REFERENCE_FIELD_NUMBER: _ClassVar[int]
    LINES_FIELD_NUMBER: _ClassVar[int]
    branch_id: str
    legal_entity_id: str
    reference: Reference
    lines: _containers.RepeatedCompositeFieldContainer[HoldLine]
    def __init__(self, branch_id: _Optional[str] = ..., legal_entity_id: _Optional[str] = ..., reference: _Optional[_Union[Reference, _Mapping]] = ..., lines: _Optional[_Iterable[_Union[HoldLine, _Mapping]]] = ...) -> None: ...

class ConfirmRequest(_message.Message):
    __slots__ = ("branch_id",)
    BRANCH_ID_FIELD_NUMBER: _ClassVar[int]
    branch_id: str
    def __init__(self, branch_id: _Optional[str] = ...) -> None: ...

class CancelRequest(_message.Message):
    __slots__ = ("branch_id",)
    BRANCH_ID_FIELD_NUMBER: _ClassVar[int]
    branch_id: str
    def __init__(self, branch_id: _Optional[str] = ...) -> None: ...

class QueryRequest(_message.Message):
    __slots__ = ("branch_id",)
    BRANCH_ID_FIELD_NUMBER: _ClassVar[int]
    branch_id: str
    def __init__(self, branch_id: _Optional[str] = ...) -> None: ...

class AdjustRequest(_message.Message):
    __slots__ = ("branch_id", "revision", "lines")
    BRANCH_ID_FIELD_NUMBER: _ClassVar[int]
    REVISION_FIELD_NUMBER: _ClassVar[int]
    LINES_FIELD_NUMBER: _ClassVar[int]
    branch_id: str
    revision: int
    lines: _containers.RepeatedCompositeFieldContainer[HoldLine]
    def __init__(self, branch_id: _Optional[str] = ..., revision: _Optional[int] = ..., lines: _Optional[_Iterable[_Union[HoldLine, _Mapping]]] = ...) -> None: ...

class BranchLine(_message.Message):
    __slots__ = ("product_id", "warehouse_id", "quantity", "consumed")
    PRODUCT_ID_FIELD_NUMBER: _ClassVar[int]
    WAREHOUSE_ID_FIELD_NUMBER: _ClassVar[int]
    QUANTITY_FIELD_NUMBER: _ClassVar[int]
    CONSUMED_FIELD_NUMBER: _ClassVar[int]
    product_id: str
    warehouse_id: str
    quantity: str
    consumed: str
    def __init__(self, product_id: _Optional[str] = ..., warehouse_id: _Optional[str] = ..., quantity: _Optional[str] = ..., consumed: _Optional[str] = ...) -> None: ...

class Shortage(_message.Message):
    __slots__ = ("product_id", "warehouse_id", "requested", "available", "reason")
    PRODUCT_ID_FIELD_NUMBER: _ClassVar[int]
    WAREHOUSE_ID_FIELD_NUMBER: _ClassVar[int]
    REQUESTED_FIELD_NUMBER: _ClassVar[int]
    AVAILABLE_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    product_id: str
    warehouse_id: str
    requested: str
    available: str
    reason: str
    def __init__(self, product_id: _Optional[str] = ..., warehouse_id: _Optional[str] = ..., requested: _Optional[str] = ..., available: _Optional[str] = ..., reason: _Optional[str] = ...) -> None: ...

class Branch(_message.Message):
    __slots__ = ("branch_id", "state", "reason", "shortages", "lines", "hold_expires_at", "version")
    BRANCH_ID_FIELD_NUMBER: _ClassVar[int]
    STATE_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    SHORTAGES_FIELD_NUMBER: _ClassVar[int]
    LINES_FIELD_NUMBER: _ClassVar[int]
    HOLD_EXPIRES_AT_FIELD_NUMBER: _ClassVar[int]
    VERSION_FIELD_NUMBER: _ClassVar[int]
    branch_id: str
    state: BranchState
    reason: str
    shortages: _containers.RepeatedCompositeFieldContainer[Shortage]
    lines: _containers.RepeatedCompositeFieldContainer[BranchLine]
    hold_expires_at: str
    version: int
    def __init__(self, branch_id: _Optional[str] = ..., state: _Optional[_Union[BranchState, str]] = ..., reason: _Optional[str] = ..., shortages: _Optional[_Iterable[_Union[Shortage, _Mapping]]] = ..., lines: _Optional[_Iterable[_Union[BranchLine, _Mapping]]] = ..., hold_expires_at: _Optional[str] = ..., version: _Optional[int] = ...) -> None: ...

class HoldResponse(_message.Message):
    __slots__ = ("branch",)
    BRANCH_FIELD_NUMBER: _ClassVar[int]
    branch: Branch
    def __init__(self, branch: _Optional[_Union[Branch, _Mapping]] = ...) -> None: ...

class ConfirmResponse(_message.Message):
    __slots__ = ("branch",)
    BRANCH_FIELD_NUMBER: _ClassVar[int]
    branch: Branch
    def __init__(self, branch: _Optional[_Union[Branch, _Mapping]] = ...) -> None: ...

class CancelResponse(_message.Message):
    __slots__ = ("branch",)
    BRANCH_FIELD_NUMBER: _ClassVar[int]
    branch: Branch
    def __init__(self, branch: _Optional[_Union[Branch, _Mapping]] = ...) -> None: ...

class QueryResponse(_message.Message):
    __slots__ = ("branch",)
    BRANCH_FIELD_NUMBER: _ClassVar[int]
    branch: Branch
    def __init__(self, branch: _Optional[_Union[Branch, _Mapping]] = ...) -> None: ...

class AdjustResponse(_message.Message):
    __slots__ = ("branch",)
    BRANCH_FIELD_NUMBER: _ClassVar[int]
    branch: Branch
    def __init__(self, branch: _Optional[_Union[Branch, _Mapping]] = ...) -> None: ...

class IssueLine(_message.Message):
    __slots__ = ("product_id", "warehouse_id", "quantity", "batch", "serials", "source_line_id")
    PRODUCT_ID_FIELD_NUMBER: _ClassVar[int]
    WAREHOUSE_ID_FIELD_NUMBER: _ClassVar[int]
    QUANTITY_FIELD_NUMBER: _ClassVar[int]
    BATCH_FIELD_NUMBER: _ClassVar[int]
    SERIALS_FIELD_NUMBER: _ClassVar[int]
    SOURCE_LINE_ID_FIELD_NUMBER: _ClassVar[int]
    product_id: str
    warehouse_id: str
    quantity: str
    batch: str
    serials: _containers.RepeatedScalarFieldContainer[str]
    source_line_id: str
    def __init__(self, product_id: _Optional[str] = ..., warehouse_id: _Optional[str] = ..., quantity: _Optional[str] = ..., batch: _Optional[str] = ..., serials: _Optional[_Iterable[str]] = ..., source_line_id: _Optional[str] = ...) -> None: ...

class IssueRequest(_message.Message):
    __slots__ = ("consumption_id", "branch_id", "business_date", "reference", "lines")
    CONSUMPTION_ID_FIELD_NUMBER: _ClassVar[int]
    BRANCH_ID_FIELD_NUMBER: _ClassVar[int]
    BUSINESS_DATE_FIELD_NUMBER: _ClassVar[int]
    REFERENCE_FIELD_NUMBER: _ClassVar[int]
    LINES_FIELD_NUMBER: _ClassVar[int]
    consumption_id: str
    branch_id: str
    business_date: str
    reference: Reference
    lines: _containers.RepeatedCompositeFieldContainer[IssueLine]
    def __init__(self, consumption_id: _Optional[str] = ..., branch_id: _Optional[str] = ..., business_date: _Optional[str] = ..., reference: _Optional[_Union[Reference, _Mapping]] = ..., lines: _Optional[_Iterable[_Union[IssueLine, _Mapping]]] = ...) -> None: ...

class IssueProblem(_message.Message):
    __slots__ = ("line", "reason", "detail")
    LINE_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    DETAIL_FIELD_NUMBER: _ClassVar[int]
    line: int
    reason: str
    detail: str
    def __init__(self, line: _Optional[int] = ..., reason: _Optional[str] = ..., detail: _Optional[str] = ...) -> None: ...

class IssueResponse(_message.Message):
    __slots__ = ("consumption_id", "issued", "movement_ids", "branch_state", "problems")
    CONSUMPTION_ID_FIELD_NUMBER: _ClassVar[int]
    ISSUED_FIELD_NUMBER: _ClassVar[int]
    MOVEMENT_IDS_FIELD_NUMBER: _ClassVar[int]
    BRANCH_STATE_FIELD_NUMBER: _ClassVar[int]
    PROBLEMS_FIELD_NUMBER: _ClassVar[int]
    consumption_id: str
    issued: bool
    movement_ids: _containers.RepeatedScalarFieldContainer[str]
    branch_state: BranchState
    problems: _containers.RepeatedCompositeFieldContainer[IssueProblem]
    def __init__(self, consumption_id: _Optional[str] = ..., issued: _Optional[bool] = ..., movement_ids: _Optional[_Iterable[str]] = ..., branch_state: _Optional[_Union[BranchState, str]] = ..., problems: _Optional[_Iterable[_Union[IssueProblem, _Mapping]]] = ...) -> None: ...

class PositionKey(_message.Message):
    __slots__ = ("product_id", "warehouse_id")
    PRODUCT_ID_FIELD_NUMBER: _ClassVar[int]
    WAREHOUSE_ID_FIELD_NUMBER: _ClassVar[int]
    product_id: str
    warehouse_id: str
    def __init__(self, product_id: _Optional[str] = ..., warehouse_id: _Optional[str] = ...) -> None: ...

class Position(_message.Message):
    __slots__ = ("product_id", "warehouse_id", "on_hand", "reserved", "available", "version")
    PRODUCT_ID_FIELD_NUMBER: _ClassVar[int]
    WAREHOUSE_ID_FIELD_NUMBER: _ClassVar[int]
    ON_HAND_FIELD_NUMBER: _ClassVar[int]
    RESERVED_FIELD_NUMBER: _ClassVar[int]
    AVAILABLE_FIELD_NUMBER: _ClassVar[int]
    VERSION_FIELD_NUMBER: _ClassVar[int]
    product_id: str
    warehouse_id: str
    on_hand: str
    reserved: str
    available: str
    version: int
    def __init__(self, product_id: _Optional[str] = ..., warehouse_id: _Optional[str] = ..., on_hand: _Optional[str] = ..., reserved: _Optional[str] = ..., available: _Optional[str] = ..., version: _Optional[int] = ...) -> None: ...

class PositionsRequest(_message.Message):
    __slots__ = ("keys",)
    KEYS_FIELD_NUMBER: _ClassVar[int]
    keys: _containers.RepeatedCompositeFieldContainer[PositionKey]
    def __init__(self, keys: _Optional[_Iterable[_Union[PositionKey, _Mapping]]] = ...) -> None: ...

class PositionsResponse(_message.Message):
    __slots__ = ("positions",)
    POSITIONS_FIELD_NUMBER: _ClassVar[int]
    positions: _containers.RepeatedCompositeFieldContainer[Position]
    def __init__(self, positions: _Optional[_Iterable[_Union[Position, _Mapping]]] = ...) -> None: ...
