package integration

// srcSchema builds a database that exercises every object kind and column type
// the tool claims to support, including the awkward ones: MAX types, all the
// date/time flavours, decimals that a float would round, CLR types, computed
// and persisted columns, filtered indexes with INCLUDE, self-referencing and
// composite foreign keys, and a view built on another view.
var srcSchema = []string{
	`CREATE SCHEMA sales`,
	`CREATE SCHEMA [odd name]`,

	`CREATE TYPE dbo.PhoneNumber FROM nvarchar(30) NOT NULL`,
	`CREATE TYPE sales.MoneyAmount FROM decimal(19,4) NULL`,
	`CREATE TYPE dbo.IdList AS TABLE ([Id] int NOT NULL PRIMARY KEY, [Note] nvarchar(50) NULL)`,

	`CREATE SEQUENCE sales.OrderNumber AS bigint START WITH 5000 INCREMENT BY 7 MINVALUE 1 MAXVALUE 999999 NO CYCLE CACHE 20`,

	`CREATE TABLE dbo.AllTypes (
		Id                int IDENTITY(10,5) NOT NULL CONSTRAINT PK_AllTypes PRIMARY KEY CLUSTERED,
		ColBit            bit NULL,
		ColTinyint        tinyint NULL,
		ColSmallint       smallint NULL,
		ColInt            int NULL,
		ColBigint         bigint NULL,
		ColDecimal        decimal(38,10) NULL,
		ColNumeric        numeric(18,4) NULL,
		ColMoney          money NULL,
		ColSmallMoney     smallmoney NULL,
		ColFloat          float(53) NULL,
		ColReal           real NULL,
		ColChar           char(10) NULL,
		ColVarchar        varchar(100) NULL,
		ColVarcharMax     varchar(max) NULL,
		ColNchar          nchar(10) NULL,
		ColNvarchar       nvarchar(100) NULL,
		ColNvarcharMax    nvarchar(max) NULL,
		ColText           text NULL,
		ColNtext          ntext NULL,
		ColBinary         binary(8) NULL,
		ColVarbinary      varbinary(50) NULL,
		ColVarbinaryMax   varbinary(max) NULL,
		ColImage          image NULL,
		ColUniqueId       uniqueidentifier NULL,
		ColDate           date NULL,
		ColTime           time(7) NULL,
		ColSmallDateTime  smalldatetime NULL,
		ColDateTime       datetime NULL,
		ColDateTime2      datetime2(7) NULL,
		ColDateTimeOffset datetimeoffset(7) NULL,
		ColXml            xml NULL,
		ColGeography      geography NULL,
		ColGeometry       geometry NULL,
		ColHierarchy      hierarchyid NULL,
		ColAliasPhone     dbo.PhoneNumber NOT NULL CONSTRAINT DF_AllTypes_Phone DEFAULT N'000',
		ColAliasMoney     sales.MoneyAmount NULL,
		ColCollated       nvarchar(50) COLLATE Latin1_General_BIN2 NULL,
		ColSparse         int SPARSE NULL,
		ColRowVersion     rowversion NOT NULL,
		ColComputed       AS (ColInt * 2),
		ColPersisted      AS (ISNULL(ColInt, 0) + 1) PERSISTED NOT NULL,
		CreatedUtc        datetime2(3) NOT NULL CONSTRAINT DF_AllTypes_Created DEFAULT SYSUTCDATETIME(),
		CONSTRAINT CK_AllTypes_Int CHECK (ColInt IS NULL OR ColInt <> 12345)
	)`,

	// Every type the bulk-copy path supports, so the importer takes that path
	// for this table and the round trip proves the bulk encoder agrees with
	// the INSERT one.
	`CREATE TABLE dbo.BulkTypes (
		Id                int IDENTITY(1,1) NOT NULL CONSTRAINT PK_BulkTypes PRIMARY KEY,
		ColBit            bit NULL,
		ColTinyint        tinyint NULL,
		ColSmallint       smallint NULL,
		ColInt            int NULL,
		ColBigint         bigint NULL,
		ColDecimal        decimal(38,10) NULL,
		ColNumeric        numeric(18,4) NULL,
		ColMoney          money NULL,
		ColSmallMoney     smallmoney NULL,
		ColFloat          float(53) NULL,
		ColReal           real NULL,
		ColChar           char(10) NULL,
		ColVarchar        varchar(100) NULL,
		ColVarcharMax     varchar(max) NULL,
		ColNchar          nchar(10) NULL,
		ColNvarchar       nvarchar(100) NULL,
		ColNvarcharMax    nvarchar(max) NULL,
		ColText           text NULL,
		ColNtext          ntext NULL,
		ColBinary         binary(8) NULL,
		ColVarbinary      varbinary(50) NULL,
		ColVarbinaryMax   varbinary(max) NULL,
		ColImage          image NULL,
		ColUniqueId       uniqueidentifier NULL,
		ColDate           date NULL,
		ColTime           time(7) NULL,
		ColSmallDateTime  smalldatetime NULL,
		ColDateTime       datetime NULL,
		ColDateTime2      datetime2(7) NULL,
		ColDateTimeOffset datetimeoffset(7) NULL,
		ColWithDefault    nvarchar(20) NULL CONSTRAINT DF_BulkTypes_D DEFAULT N'default-value'
	)`,

	`CREATE TABLE sales.Customer (
		CustomerId  int IDENTITY(1,1) NOT NULL,
		Code        varchar(20) NOT NULL,
		Name        nvarchar(200) NOT NULL,
		ParentId    int NULL,
		Balance     decimal(19,4) NOT NULL CONSTRAINT DF_Customer_Balance DEFAULT ((0)),
		IsActive    bit NOT NULL CONSTRAINT DF_Customer_Active DEFAULT ((1)),
		RowGuid     uniqueidentifier ROWGUIDCOL NOT NULL CONSTRAINT DF_Customer_Guid DEFAULT NEWID(),
		CONSTRAINT PK_Customer PRIMARY KEY CLUSTERED (CustomerId),
		CONSTRAINT UQ_Customer_Code UNIQUE NONCLUSTERED (Code),
		CONSTRAINT FK_Customer_Parent FOREIGN KEY (ParentId) REFERENCES sales.Customer (CustomerId)
	)`,

	`CREATE TABLE sales.[Order Line] (
		OrderId    int NOT NULL,
		LineNumber     smallint NOT NULL,
		CustomerId int NOT NULL,
		Sku        varchar(40) NOT NULL,
		Qty        int NOT NULL CONSTRAINT CK_OrderLine_Qty CHECK (Qty > 0),
		UnitPrice  decimal(19,4) NOT NULL,
		LineTotal  AS (Qty * UnitPrice),
		CONSTRAINT PK_OrderLine PRIMARY KEY CLUSTERED (OrderId, LineNumber),
		CONSTRAINT FK_OrderLine_Customer FOREIGN KEY (CustomerId)
			REFERENCES sales.Customer (CustomerId) ON DELETE CASCADE ON UPDATE NO ACTION
	)`,

	`CREATE TABLE [odd name].[Weird ""Table""] (
		[Key] nvarchar(50) NOT NULL CONSTRAINT [PK_Weird Table] PRIMARY KEY,
		[Value ""quoted""] nvarchar(max) NULL
	)`,

	`CREATE INDEX IX_Customer_Name ON sales.Customer (Name DESC) INCLUDE (Balance, IsActive) WITH (FILLFACTOR = 80)`,
	`CREATE UNIQUE INDEX IX_Customer_ActiveCode ON sales.Customer (Code) WHERE IsActive = 1`,
	`CREATE INDEX IX_OrderLine_Sku ON sales.[Order Line] (Sku ASC, LineNumber DESC)`,

	`CREATE VIEW sales.ActiveCustomer AS
	SELECT CustomerId, Code, Name, Balance FROM sales.Customer WHERE IsActive = 1`,

	`CREATE VIEW sales.ActiveCustomerTotals AS
	SELECT c.CustomerId, c.Name, SUM(ISNULL(l.LineTotal, 0)) AS Total
	FROM sales.ActiveCustomer c
	LEFT JOIN sales.[Order Line] l ON l.CustomerId = c.CustomerId
	GROUP BY c.CustomerId, c.Name`,

	`CREATE FUNCTION dbo.DoubleIt (@n int) RETURNS int AS BEGIN RETURN @n * 2 END`,

	`CREATE FUNCTION sales.CustomersOver (@min decimal(19,4))
	RETURNS TABLE AS RETURN (SELECT CustomerId, Name FROM sales.Customer WHERE Balance >= @min)`,

	`CREATE FUNCTION sales.CustomerNames (@min decimal(19,4))
	RETURNS @r TABLE (CustomerId int, Name nvarchar(200)) AS
	BEGIN
		INSERT INTO @r SELECT CustomerId, Name FROM sales.Customer WHERE Balance >= @min
		RETURN
	END`,

	`CREATE PROCEDURE sales.GetCustomer @id int AS
	BEGIN
		SET NOCOUNT ON
		SELECT * FROM sales.ActiveCustomerTotals WHERE CustomerId = @id
	END`,

	`CREATE PROCEDURE sales.BulkNote @ids dbo.IdList READONLY AS
	BEGIN
		SET NOCOUNT ON
		SELECT COUNT(*) FROM @ids
	END`,

	`CREATE TRIGGER sales.TR_Customer_Audit ON sales.Customer AFTER UPDATE AS
	BEGIN
		SET NOCOUNT ON
	END`,
}

// srcData fills the tables with values chosen to break naive encoders.
var srcData = []string{
	// IDENTITY_INSERT is session state, so it has to share a batch with the
	// insert it enables.
	`SET IDENTITY_INSERT sales.Customer ON
	INSERT INTO sales.Customer (CustomerId, Code, Name, ParentId, Balance, IsActive, RowGuid) VALUES
		(1, 'ACME',  N'Acme Ltd',            NULL, 1250.5000,  1, '3F2504E0-4F89-11D3-9A0C-0305E82C3301'),
		(2, 'BOHM',  N'Böhm & Co ''quoted''',   1, -3.5000,    1, '00000000-0000-0000-0000-000000000000'),
		(3, 'UNI',   N'Ünïcödé 日本語 🎉',       1, 0.0001,     0, NEWID()),
		(4, 'EMPTY', N'',                     NULL, 0.0000,    1, NEWID())
	SET IDENTITY_INSERT sales.Customer OFF`,

	`INSERT INTO sales.[Order Line] (OrderId, LineNumber, CustomerId, Sku, Qty, UnitPrice) VALUES
		(1, 1, 1, 'SKU-1', 3, 19.9900),
		(1, 2, 1, 'SKU-2', 1, 100.0000),
		(2, 1, 2, 'SKU-1', 7, 0.0100)`,

	`INSERT INTO [odd name].[Weird ""Table""] ([Key], [Value ""quoted""]) VALUES
		(N'a', N'line1' + CHAR(13) + CHAR(10) + N'line2' + CHAR(9) + N'tabbed'),
		(N'b', NULL),
		(N'c', N'')`,

	// A fully populated row, with values at or near the limits of each type.
	`SET IDENTITY_INSERT dbo.AllTypes ON
	INSERT INTO dbo.AllTypes (
		Id, ColBit, ColTinyint, ColSmallint, ColInt, ColBigint,
		ColDecimal, ColNumeric, ColMoney, ColSmallMoney, ColFloat, ColReal,
		ColChar, ColVarchar, ColVarcharMax, ColNchar, ColNvarchar, ColNvarcharMax,
		ColText, ColNtext, ColBinary, ColVarbinary, ColVarbinaryMax, ColImage,
		ColUniqueId, ColDate, ColTime, ColSmallDateTime, ColDateTime, ColDateTime2, ColDateTimeOffset,
		ColXml, ColGeography, ColGeometry, ColHierarchy,
		ColAliasPhone, ColAliasMoney, ColCollated, ColSparse
	) VALUES (
		1, 1, 255, -32768, -2147483648, 9223372036854775807,
		-1234567890123456789012345678.1234567890, 12345678901234.5678, 922337203685477.5807, -214748.3648,
		1.7976931348623157E+308, 1.18e-38,
		'padded    ', 'ascii ''quoted''', REPLICATE('x', 9000), N'nchar     ', N'Ünïcödé', REPLICATE(N'ü', 9000),
		'legacy text', N'legacy ntext', 0x0011223344556677, 0xDEADBEEF, 0x, 0xCAFEBABE,
		'3F2504E0-4F89-11D3-9A0C-0305E82C3301', '0001-01-01', '23:59:59.9999999',
		'1900-01-01T00:00:00', '1753-01-01T00:00:00.003', '9999-12-31T23:59:59.9999999',
		'2026-08-27T13:45:00.1234567+02:00',
		N'<root a="1"><child>text &amp; more</child></root>',
		geography::STGeomFromText('POINT(5.1214 52.0907)', 4326),
		geometry::STGeomFromText('LINESTRING(0 0, 10 10)', 0),
		hierarchyid::Parse('/1/2/'),
		N'+31 6 12345678', 99.9999, N'BINARY collated', 42
	)
	SET IDENTITY_INSERT dbo.AllTypes OFF`,

	// An all-NULL row: this is the one that catches parameter-typing bugs.
	`INSERT INTO dbo.AllTypes (ColAliasPhone) VALUES (N'nulls')`,

	// The same three shapes for the bulk-copy path. The NULL row matters most:
	// bulk copy without KEEP_NULLS would silently substitute ColWithDefault's
	// default, and an empty string written as NULL (or the reverse) would show
	// up in the comparison.
	`SET IDENTITY_INSERT dbo.BulkTypes ON
	INSERT INTO dbo.BulkTypes (
		Id, ColBit, ColTinyint, ColSmallint, ColInt, ColBigint,
		ColDecimal, ColNumeric, ColMoney, ColSmallMoney, ColFloat, ColReal,
		ColChar, ColVarchar, ColVarcharMax, ColNchar, ColNvarchar, ColNvarcharMax,
		ColText, ColNtext, ColBinary, ColVarbinary, ColVarbinaryMax, ColImage,
		ColUniqueId, ColDate, ColTime, ColSmallDateTime, ColDateTime, ColDateTime2,
		ColDateTimeOffset, ColWithDefault
	) VALUES (
		7, 1, 255, -32768, -2147483648, 9223372036854775807,
		-1234567890123456789012345678.1234567890, 12345678901234.5678, 922337203685477.5807, -214748.3648,
		1.7976931348623157E+308, 1.18e-38,
		'padded    ', 'ascii ''quoted''', REPLICATE('x', 9000), N'nchar     ', N'Ünïcödé 日本語 🎉', REPLICATE(N'ü', 9000),
		'legacy text', N'legacy ntext', 0x0011223344556677, 0xDEADBEEF, 0x, 0xCAFEBABE,
		'3F2504E0-4F89-11D3-9A0C-0305E82C3301', '0001-01-01', '23:59:59.9999999',
		'1900-01-01T00:00:00', '1753-01-01T00:00:00.003', '9999-12-31T23:59:59.9999999',
		'2026-08-27T13:45:00.1234567+02:00', N'explicit'
	)
	SET IDENTITY_INSERT dbo.BulkTypes OFF`,

	`INSERT INTO dbo.BulkTypes (ColWithDefault) VALUES (NULL)`,

	`INSERT INTO dbo.BulkTypes (
		ColBit, ColTinyint, ColSmallint, ColInt, ColBigint, ColDecimal, ColNumeric,
		ColMoney, ColSmallMoney, ColFloat, ColReal, ColChar, ColVarchar, ColVarcharMax,
		ColNchar, ColNvarchar, ColNvarcharMax, ColBinary, ColVarbinary, ColVarbinaryMax,
		ColUniqueId, ColDate, ColTime, ColSmallDateTime, ColDateTime, ColDateTime2,
		ColDateTimeOffset, ColWithDefault
	) VALUES (
		0, 0, 0, 0, 0, 0.0000000000, 0.0000, 0.0000, 0.0000, 0, 0,
		'', '', '', N'', N'', N'', 0x0000000000000000, 0x, 0x,
		'00000000-0000-0000-0000-000000000000', '2026-02-28', '00:00:00',
		'2026-01-01T00:00:00', '2026-01-01T00:00:00', '2026-01-01T00:00:00',
		'2026-01-01T00:00:00-11:00', N''
	)`,

	// Zero and empty-string edge cases distinct from NULL.
	`INSERT INTO dbo.AllTypes (
		ColBit, ColTinyint, ColSmallint, ColInt, ColBigint, ColDecimal, ColNumeric,
		ColMoney, ColSmallMoney, ColFloat, ColReal, ColChar, ColVarchar, ColVarcharMax,
		ColNchar, ColNvarchar, ColNvarcharMax, ColVarbinary, ColUniqueId,
		ColDate, ColTime, ColDateTime, ColDateTime2, ColDateTimeOffset, ColAliasPhone
	) VALUES (
		0, 0, 0, 0, 0, 0.0000000000, 0.0000, 0.0000, 0.0000, 0, 0,
		'', '', '', N'', N'', N'', 0x, '00000000-0000-0000-0000-000000000000',
		'2026-02-28', '00:00:00', '2026-01-01T00:00:00', '2026-01-01T00:00:00',
		'2026-01-01T00:00:00-11:00', N'0'
	)`,
}
