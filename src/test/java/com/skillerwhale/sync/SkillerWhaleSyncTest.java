package com.skillerwhale.sync;

import static org.junit.jupiter.api.Assertions.*;

import java.io.IOException;
import java.io.File;
import java.nio.file.*;
import java.util.*;
import java.util.stream.*;

import org.junit.jupiter.api.io.TempDir;
import org.junit.jupiter.params.*;
import org.junit.jupiter.params.provider.*;

// import static com.github.tomakehurst.wiremock.client.WireMock.*;

class SkillerWhaleSyncTest {

	@TempDir
	Path testDir;

	@ParameterizedTest(name = "test_jsonStringQuote({0}) = {1}")
	@MethodSource("jsonStrings")
	void test_jsonStringQuote(String expected, String actual) throws java.io.IOException {
		assertEquals(expected, SkillerWhaleSync.jsonStringQuote(actual));
	}

	private static Stream<Arguments> jsonStrings() {
		return Stream.of(
			Arguments.of("\"horse\\nor\\nlaminator\"", "horse\nor\nlaminator"),
			Arguments.of("\"\\tIt was a dark and stormy night\"", "\tIt was a dark and stormy night"),
			Arguments.of("\"❤ Capybaras! ❤\"", "❤ Capybaras! ❤"),
			Arguments.of("\"\\u0007 Licence to beep \\n\"", "\u0007 Licence to beep \n"),
			Arguments.of("\"\\\\\\\\\"", "\\\\")
		);
	}

	@ParameterizedTest(name = "test_getenvAndSplit({0}) = {1}")
	@MethodSource("envSplitStrings")
	void test_getenvAndSplit(String[] expected, String test) {
		var env = new HashMap<String,String>();
		env.put("TEST", "foo");
		assertArrayEquals(new String[]{"foo"}, SkillerWhaleSync.getenvAndSplit(env, "TEST"));
	}

	private static Stream<Arguments> envSplitStrings() {
		return Stream.of(
			Arguments.of(new String[]{"go","java","cpp"},"go java cpp"),
			Arguments.of(new String[]{"go","java","cpp"},"         go java cpp         "),
			Arguments.of(new String[]{"go","java","cpp"},"         \"go\" java cpp         "),
			Arguments.of(new String[]{"go","java","cpp"},"[\"go\", \"java\",    \"cpp\"   ]")
		);
	}

	// @WireMockTest
	// void test_ping() {
	// 	assertEquals(true, true);
	// }

	// private SkillerWhaleSync testInstance() throws IOException {
	//  	Path watching = Paths.get(testDir.toString() + File.separator + "watching");
	//  	Path attendanceIdFile = Paths.get(testDir.toString() + File.separator + "attendance_id");
	//  	return new SkillerWhaleSync("undefined", attendanceIdFile, "http://localhost:12345", watching, new String[]{"test"}, null);
	// }
}
