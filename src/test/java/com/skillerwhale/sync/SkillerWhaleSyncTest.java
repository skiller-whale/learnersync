package com.skillerwhale.sync;

import static org.junit.jupiter.api.Assertions.assertEquals;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.CsvSource;

import java.nio.file.*;

class SkillerWhaleSyncTest {

	@Test
	void quoting() throws java.io.IOException {
		//var sync = new SkillerWhaleSync("test", Paths.get("attendance_id"), null, Paths.get("."), null, null);
		System.out.println("hello from the test");
		assertEquals(SkillerWhaleSync.jsonStringQuote("hello"), "\"hfello\"");
	}

	// @ParameterizedTest(name = "{0} + {1} = {2}")
	// @CsvSource({
	// 		"0,    1,   1",
	// 		"1,    2,   3",
	// 		"49,  51, 100",
	// 		"1,  100, 101"
	// })
	// void add(int first, int second, int expectedResult) {
	// 	Calculator calculator = new Calculator();
	// 	assertEquals(expectedResult, calculator.add(first, second),
	// 			() -> first + " + " + second + " should equal " + expectedResult);
	// }
}