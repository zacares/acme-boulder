-- +migrate Up
-- SQL in section 'Up' is executed when this migration is applied

-- The orders2 table holds one row per ACME Order object. The authorizations
-- column contains an opaque JSON blob which the SA can use to find the
-- associated authorizations without requiring db-level foreign keys. Most
-- orders are created with status "pending", but may be created with status
-- "ready" if all of their authorizations are reused and already valid. Orders
-- transition to status "processing" when finalization begins. The error field
-- is populated only if an error occurs during finalization and the order moves
-- to the "invalid" state; errors during validation are reflected elsewhere.
CREATE TABLE `orders2` (
  `id` bigint(20) UNSIGNED NOT NULL AUTO_INCREMENT,
  `registrationID` bigint(20) NOT NULL,
  `created` datetime NOT NULL,
  `expires` datetime NOT NULL,
  `authorizations` json NOT NULL,
  `profile` varchar(255) NOT NULL,
  `status` tinyint(4) NOT NULL,
  `error` mediumblob DEFAULT NULL,
  `certificateSerial` varchar(255) DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `reg_status_expires` (`registrationID`,`expires`),
  KEY `regID_created_idx` (`registrationID`,`created`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
 PARTITION BY RANGE(id)
(PARTITION p_start VALUES LESS THAN (MAXVALUE));

-- The authorizations table holds one row per ACME Authorization object and
-- associated challenges. It is always created with status "pending". After
-- one of its challenges is attempted, it will transition into either status
-- "valid" or "invalid", and the validations column will be updated to point
-- to a new row in the validations table containing the record of that attempt.
CREATE TABLE `authorizations` (
  `id` bigint(20) UNSIGNED NOT NULL AUTO_INCREMENT,
  `registrationID` bigint(20) NOT NULL,
  `identifierType` tinyint(4) NOT NULL,
  `identifierValue` varchar(255) NOT NULL,
  `created` datetime NOT NULL,
  `expires` datetime NOT NULL,
  `profile` varchar(255) NOT NULL,
  `challenges` tinyint(4) NOT NULL,
  `token` binary(32) NOT NULL,
  `status` tinyint(4) NOT NULL,
  `validations` json DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `regID_expires_idx` (`registrationID`,`status`,`expires`),
  KEY `regID_identifier_status_expires_idx` (`registrationID`,`identifierType`,`identifierValue`,`status`,`expires`),
  KEY `expires_idx` (`expires`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
 PARTITION BY RANGE(id)
(PARTITION p_start VALUES LESS THAN (MAXVALUE));

-- The validations table holds records of completed validation attempts,
-- including the validation method used, the resulting status (valid or
-- invalid), and an opaque blob of our audit record.
CREATE TABLE `validations` (
  `id` bigint(20) UNSIGNED NOT NULL AUTO_INCREMENT,
  `challenge` tinyint(4) NOT NULL,
  `attemptedAt` datetime NOT NULL,
  `status` tinyint(4) NOT NULL,
  `record` json DEFAULT NULL,
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
 PARTITION BY RANGE(id)
(PARTITION p_start VALUES LESS THAN (MAXVALUE));

-- +migrate Down
-- SQL section 'Down' is executed when this migration is rolled back

DROP TABLE `validations`;
DROP TABLE `authorizations`;
DROP TABLE `orders2`;
