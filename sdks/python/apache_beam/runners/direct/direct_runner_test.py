#
# Licensed to the Apache Software Foundation (ASF) under one or more
# contributor license agreements.  See the NOTICE file distributed with
# this work for additional information regarding copyright ownership.
# The ASF licenses this file to You under the Apache License, Version 2.0
# (the "License"); you may not use this file except in compliance with
# the License.  You may obtain a copy of the License at
#
#    http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#

# pytype: skip-file

import threading
import unittest
from collections import defaultdict

import hamcrest as hc

import apache_beam as beam
from apache_beam.metrics.cells import DistributionData
from apache_beam.metrics.cells import DistributionResult
from apache_beam.metrics.execution import MetricKey
from apache_beam.metrics.execution import MetricResult
from apache_beam.metrics.metric import Metrics
from apache_beam.metrics.metric import MetricsFilter
from apache_beam.metrics.metricbase import MetricName
from apache_beam.pipeline import Pipeline
from apache_beam.runners import DirectRunner
from apache_beam.runners import TestDirectRunner
from apache_beam.runners import create_runner
from apache_beam.runners.direct.evaluation_context import _ExecutionContext
from apache_beam.runners.direct.transform_evaluator import _GroupByKeyOnlyEvaluator
from apache_beam.runners.direct.transform_evaluator import _TransformEvaluator
from apache_beam.testing import test_pipeline
from apache_beam.testing.util import assert_that
from apache_beam.testing.util import equal_to
from apache_beam.utils import shared


class DictWrapper(dict):
  pass


def acquire_dict():
  return DictWrapper()


class DirectPipelineResultTest(unittest.TestCase):
  def test_waiting_on_result_stops_executor_threads(self):
    pre_test_threads = set(t.ident for t in threading.enumerate())

    for runner in [
        'BundleBasedDirectRunner',
        'apache_beam.runners.portability.fn_api_runner.fn_runner.FnApiRunner'
    ]:
      pipeline = test_pipeline.TestPipeline(runner=runner)
      _ = (pipeline | beam.Create([{'foo': 'bar'}]))
      result = pipeline.run()
      result.wait_until_finish()

      post_test_threads = set(t.ident for t in threading.enumerate())
      new_threads = post_test_threads - pre_test_threads
      self.assertEqual(len(new_threads), 0)

  def test_direct_runner_metrics(self):
    class MyDoFn(beam.DoFn):
      def start_bundle(self):
        count = Metrics.counter(self.__class__, 'bundles')
        count.inc()

      def finish_bundle(self):
        count = Metrics.counter(self.__class__, 'finished_bundles')
        count.inc()

      def process(self, element):
        gauge = Metrics.gauge(self.__class__, 'latest_element')
        gauge.set(element)
        count = Metrics.counter(self.__class__, 'elements')
        count.inc()
        distro = Metrics.distribution(self.__class__, 'element_dist')
        distro.update(element)
        str_set = Metrics.string_set(self.__class__, 'element_str_set')
        str_set.add(str(element % 4))
        Metrics.bounded_trie(self.__class__, 'element_bounded_trie').add(
            ("a", "b", str(element % 4)))
        return [element]

    # TODO(https://github.com/apache/beam/issues/34549): This test relies on
    # metrics filtering which doesn't work on Prism yet because Prism renames
    # steps (e.g. "Do" becomes "ref_AppliedPTransform_Do_7").
    # https://github.com/apache/beam/blob/5f9cd73b7c9a2f37f83971ace3a399d633201dd1/sdks/python/apache_beam/runners/portability/fn_api_runner/fn_runner.py#L1590
    p = Pipeline('FnApiRunner')
    pcoll = (
        p | beam.Create([1, 2, 3, 4, 5], reshuffle=False)
        | 'Do' >> beam.ParDo(MyDoFn()))
    assert_that(pcoll, equal_to([1, 2, 3, 4, 5]))
    result = p.run()
    result.wait_until_finish()
    metrics = result.metrics().query(MetricsFilter().with_step('Do'))
    namespace = '{}.{}'.format(MyDoFn.__module__, MyDoFn.__name__)

    hc.assert_that(
        metrics['counters'],
        hc.contains_inanyorder(
            MetricResult(
                MetricKey('Do', MetricName(namespace, 'elements')), 5, 5),
            MetricResult(
                MetricKey('Do', MetricName(namespace, 'bundles')), 1, 1),
            MetricResult(
                MetricKey('Do', MetricName(namespace, 'finished_bundles')),
                1,
                1)))

    hc.assert_that(
        metrics['distributions'],
        hc.contains_inanyorder(
            MetricResult(
                MetricKey('Do', MetricName(namespace, 'element_dist')),
                DistributionResult(DistributionData(15, 5, 1, 5)),
                DistributionResult(DistributionData(15, 5, 1, 5)))))

    gauge_result = metrics['gauges'][0]
    hc.assert_that(
        gauge_result.key,
        hc.equal_to(MetricKey('Do', MetricName(namespace, 'latest_element'))))
    hc.assert_that(gauge_result.committed.value, hc.equal_to(5))
    hc.assert_that(gauge_result.attempted.value, hc.equal_to(5))

    str_set_result = metrics['string_sets'][0]
    hc.assert_that(
        str_set_result.key,
        hc.equal_to(MetricKey('Do', MetricName(namespace, 'element_str_set'))))
    hc.assert_that(len(str_set_result.committed), hc.equal_to(4))
    hc.assert_that(len(str_set_result.attempted), hc.equal_to(4))

    bounded_trie_results = metrics['bounded_tries'][0]
    hc.assert_that(
        bounded_trie_results.key,
        hc.equal_to(
            MetricKey('Do', MetricName(namespace, 'element_bounded_trie'))))
    hc.assert_that(bounded_trie_results.committed.size(), hc.equal_to(4))
    hc.assert_that(bounded_trie_results.attempted.size(), hc.equal_to(4))

  def test_create_runner(self):
    self.assertTrue(isinstance(create_runner('DirectRunner'), DirectRunner))
    self.assertTrue(
        isinstance(create_runner('TestDirectRunner'), TestDirectRunner))


class BundleBasedRunnerTest(unittest.TestCase):
  def test_type_hints(self):
    with test_pipeline.TestPipeline(runner='BundleBasedDirectRunner') as p:
      _ = (
          p
          | beam.Create([[]]).with_output_types(beam.typehints.List[int])
          | beam.combiners.Count.Globally())

  def test_impulse(self):
    with test_pipeline.TestPipeline(runner='BundleBasedDirectRunner') as p:
      assert_that(p | beam.Impulse(), equal_to([b'']))


class DirectRunnerRetryTests(unittest.TestCase):
  def test_retry_fork_graph(self):
    # TODO(https://github.com/apache/beam/issues/18640): The FnApiRunner
    # currently does not currently support retries.
    p = beam.Pipeline(runner='BundleBasedDirectRunner')

    shared_handler = shared.Shared()
    counts = shared_handler.acquire(acquire_dict)
    counts['count_b'] = 0
    counts['count_c'] = 0

    def f_b(x):
      shared_handler.acquire(acquire_dict)['count_b'] += 1
      raise Exception('exception in f_b')

    def f_c(x):
      shared_handler.acquire(acquire_dict)['count_c'] += 1
      raise Exception('exception in f_c')

    names = p | 'CreateNodeA' >> beam.Create(['Ann', 'Joe'])

    fork_b = names | 'SendToB' >> beam.Map(f_b)  # pylint: disable=unused-variable
    fork_c = names | 'SendToC' >> beam.Map(f_c)  # pylint: disable=unused-variable

    with self.assertRaises(Exception):
      p.run().wait_until_finish()
    assert counts['count_b'] == counts['count_c'] == 4

  def test_no_partial_writeouts(self):
    class TestTransformEvaluator(_TransformEvaluator):
      def __init__(self):
        self._execution_context = _ExecutionContext(None, {})

      def start_bundle(self):
        self.step_context = self._execution_context.get_step_context()

      def process_element(self, element):
        k, v = element
        state = self.step_context.get_keyed_state(k)
        state.add_state(None, _GroupByKeyOnlyEvaluator.ELEMENTS_TAG, v)

    # Create instance and add key/value, key/value2
    evaluator = TestTransformEvaluator()
    evaluator.start_bundle()
    self.assertIsNone(evaluator.step_context.existing_keyed_state.get('key'))
    self.assertIsNone(evaluator.step_context.partial_keyed_state.get('key'))

    evaluator.process_element(['key', 'value'])
    self.assertEqual(
        evaluator.step_context.existing_keyed_state['key'].state,
        defaultdict(lambda: defaultdict(list)))
    self.assertEqual(
        evaluator.step_context.partial_keyed_state['key'].state,
        {None: {
            'elements': ['value']
        }})

    evaluator.process_element(['key', 'value2'])
    self.assertEqual(
        evaluator.step_context.existing_keyed_state['key'].state,
        defaultdict(lambda: defaultdict(list)))
    self.assertEqual(
        evaluator.step_context.partial_keyed_state['key'].state,
        {None: {
            'elements': ['value', 'value2']
        }})

    # Simulate an exception (redo key/value)
    evaluator._execution_context.reset()
    evaluator.start_bundle()
    evaluator.process_element(['key', 'value'])
    self.assertEqual(
        evaluator.step_context.existing_keyed_state['key'].state,
        defaultdict(lambda: defaultdict(list)))
    self.assertEqual(
        evaluator.step_context.partial_keyed_state['key'].state,
        {None: {
            'elements': ['value']
        }})


if __name__ == '__main__':
  unittest.main()
