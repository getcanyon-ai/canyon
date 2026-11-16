// React Testing Library unmounts between tests only when something schedules it, and without
// vitest globals there is no implicit afterEach to hook. One setup file does it for every test.
import "./setup-dom";

// Clock times and day groups read the same on every machine: a test that says 14:20 means UTC.
process.env.TZ = "UTC";
import { cleanup } from "@testing-library/react";
import { afterEach } from "vitest";

afterEach(cleanup);
