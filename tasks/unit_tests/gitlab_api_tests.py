import unittest

from invoke.context import MockContext

from tasks.libs.ciproviders.gitlab_api import (
    GitlabCIDiff,
    clean_gitlab_ci_configuration,
    filter_gitlab_ci_configuration,
    read_includes,
)


class TestReadIncludes(unittest.TestCase):
    def test_with_includes(self):
        includes = []
        read_includes(MockContext(), "tasks/unit_tests/testdata/in.yml", includes)
        self.assertEqual(len(includes), 4)

    def test_without_includes(self):
        includes = []
        read_includes(MockContext(), "tasks/unit_tests/testdata/b.yml", includes)
        self.assertEqual(len(includes), 1)


class TestGitlabCiConfig(unittest.TestCase):
    def test_filter(self):
        yml = {
            '.wrapper': {'before_script': 'echo "start"'},
            'job1': {'script': 'echo "hello"'},
            'job2': {'script': 'echo "world"'},
        }
        expected_yml = {
            'job1': {'script': 'echo "hello"'},
            'job2': {'script': 'echo "world"'},
        }

        res = filter_gitlab_ci_configuration(yml)

        self.assertDictEqual(res, expected_yml)

    def test_filter_job(self):
        yml = {
            '.wrapper': {'before_script': 'echo "start"'},
            'job1': {'script': 'echo "hello"'},
            'job2': {'script': 'echo "world"'},
        }
        expected_yml = {
            'job1': {'script': 'echo "hello"'},
        }

        res = filter_gitlab_ci_configuration(yml, job='job1')

        self.assertDictEqual(res, expected_yml)

    def test_clean_nop(self):
        yml = {
            'job': {'script': ['echo hello']},
        }
        expected_yml = {
            'job': {'script': ['echo hello']},
        }
        res = clean_gitlab_ci_configuration(yml)

        self.assertDictEqual(res, expected_yml)

    def test_clean_flatten_nest1(self):
        yml = {
            'job': {
                'script': [
                    [
                        'echo hello',
                        'echo world',
                    ],
                    'echo "!"',
                ]
            },
        }
        expected_yml = {
            'job': {
                'script': [
                    'echo hello',
                    'echo world',
                    'echo "!"',
                ]
            },
        }
        res = clean_gitlab_ci_configuration(yml)

        self.assertDictEqual(res, expected_yml)

    def test_clean_flatten_nest2(self):
        yml = {
            'job': {
                'script': [
                    [
                        [['echo i am nested']],
                        'echo hello',
                        'echo world',
                    ],
                    'echo "!"',
                ]
            },
        }
        expected_yml = {
            'job': {
                'script': [
                    'echo i am nested',
                    'echo hello',
                    'echo world',
                    'echo "!"',
                ]
            },
        }
        res = clean_gitlab_ci_configuration(yml)

        self.assertDictEqual(res, expected_yml)

    def test_clean_extends(self):
        yml = {
            'job': {'extends': '.mywrapper', 'script': ['echo hello']},
        }
        expected_yml = {
            'job': {'script': ['echo hello']},
        }
        res = clean_gitlab_ci_configuration(yml)

        self.assertDictEqual(res, expected_yml)


class TestGitlabCiDiff(unittest.TestCase):
    def test_make_diff(self):
        before = {
            'job1': {
                'script': [
                    'echo "hello"',
                    'echo "hello?"',
                    'echo "hello!"',
                ]
            },
            'job2': {
                'script': 'echo "world"',
            },
            'job3': {
                'script': 'echo "!"',
            },
            'job4': {
                'script': 'echo "?"',
            },
        }
        after = {
            'job1': {
                'script': [
                    'echo "hello"',
                    'echo "bonjour?"',
                    'echo "hello!"',
                ]
            },
            'job2_renamed': {
                'script': 'echo "world"',
            },
            'job3': {
                'script': 'echo "!"',
            },
            'job5': {
                'script': 'echo "???"',
            },
        }
        diff = GitlabCIDiff(before, after)
        self.assertSetEqual(diff.modified, {'job1'})
        self.assertSetEqual(set(diff.modified_diffs.keys()), {'job1'})
        self.assertSetEqual(diff.removed, {'job4'})
        self.assertSetEqual(diff.added, {'job5'})
        self.assertSetEqual(diff.renamed, {('job2', 'job2_renamed')})
